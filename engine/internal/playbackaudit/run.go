// Package playbackaudit verifies one finite media transfer with a paced software
// decoder. Decoded frames go to a null sink, not a display or speaker.
package playbackaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/mediaprobe"
)

type Config struct {
	RootCAs                 *x509.CertPool `json:"-"`
	ProxyURL                string         `json:"-"`
	URL, Client, Decoder    string
	Timeout, RequestTimeout time.Duration
}

type Resource struct {
	CPUAvailable      bool    `json:"cpu_available"`
	UserSeconds       float64 `json:"user_seconds"`
	SystemSeconds     float64 `json:"system_seconds"`
	CPUPercentOneCore float64 `json:"cpu_percent_one_core"`
	PeakRSSBytes      *int64  `json:"peak_rss_bytes,omitempty"`
}

type DecoderReport struct {
	Version      string      `json:"version"`
	BinarySHA256 string      `json:"binary_sha256"`
	Arguments    []string    `json:"arguments"`
	ExitCode     int         `json:"exit_code"`
	Stats        DecodeStats `json:"stats"`
	Resource     Resource    `json:"resource"`
}

type Report struct {
	StartedAt                 time.Time               `json:"started_at"`
	EndedAt                   time.Time               `json:"ended_at"`
	ElapsedSeconds            float64                 `json:"elapsed_seconds"`
	Scope                     string                  `json:"scope"`
	StopReason                string                  `json:"stop_reason"`
	Completed                 bool                    `json:"completed"`
	Stream                    mediaprobe.StreamReport `json:"stream"`
	Decoder                   DecoderReport           `json:"decoder"`
	GoResource                Resource                `json:"go_resource"`
	CombinedCPUPercentOneCore *float64                `json:"combined_cpu_percent_one_core,omitempty"`
	StallMeasurement          string                  `json:"stall_measurement"`
	Coverage                  Coverage                `json:"duration_coverage"`
}

type Coverage struct {
	ManifestSeconds  float64  `json:"manifest_seconds"`
	VideoSeconds     *float64 `json:"decoded_video_seconds,omitempty"`
	AudioSeconds     float64  `json:"decoded_audio_seconds"`
	ToleranceSeconds float64  `json:"tolerance_seconds"`
	VideoMatches     bool     `json:"video_matches"`
	AudioMatches     bool     `json:"audio_matches"`
}

func ValidateConfig(cfg Config) error {
	if cfg.Timeout <= 0 || cfg.Timeout > 2*time.Minute {
		return errors.New("total timeout must be positive and at most 2m")
	}
	if cfg.RequestTimeout <= 0 {
		return errors.New("request timeout must be positive")
	}
	if err := mediaprobe.ValidateStreamConfig(mediaprobe.StreamConfig{URL: cfg.URL, Client: cfg.Client, Timeout: cfg.RequestTimeout, ProxyURL: cfg.ProxyURL, RootCAs: cfg.RootCAs}); err != nil {
		return err
	}
	if cfg.Decoder == "" {
		return errors.New("FFmpeg decoder is required")
	}
	if _, err := exec.LookPath(cfg.Decoder); err != nil {
		return errors.New("FFmpeg executable not found; pass --decoder /absolute/path/to/ffmpeg")
	}
	return nil
}

func Run(ctx context.Context, cfg Config) (Report, error) {
	return run(ctx, cfg, mediaprobe.StreamVOD)
}

type streamer func(context.Context, mediaprobe.StreamConfig, io.Writer) (mediaprobe.StreamReport, error)

func run(parent context.Context, cfg Config, stream streamer) (report Report, err error) {
	if parent == nil || stream == nil {
		return report, errors.New("context and stream source are required")
	}
	if err := ValidateConfig(cfg); err != nil {
		return report, err
	}
	return runValidated(parent, cfg, stream)
}

// runValidated owns the decoder lifecycle after the public configuration checks.
// The opt-in loopback load fixture uses it for a longer, bounded media timeline;
// public Run and the CLI retain their two-minute deadline limit.
func runValidated(parent context.Context, cfg Config, stream streamer) (report Report, err error) {
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()
	decoder, err := exec.LookPath(cfg.Decoder)
	if err != nil {
		return report, errors.New("decoder executable unavailable")
	}
	decoder, err = filepath.Abs(decoder)
	if err != nil {
		return report, errors.New("decoder path unavailable")
	}
	identity, version, err := decoderIdentity(ctx, decoder)
	if err != nil {
		return report, err
	}
	args := []string{
		"-hide_banner", "-nostdin", "-nostats", "-loglevel", "level+info",
		"-xerror", "-max_error_rate", "0", "-stats_period", "0.25", "-progress", "pipe:1",
		// FFmpeg 7.1 treats zero initial burst as its 0.5s default. A small
		// positive value requests a negligible, explicit startup burst.
		"-protocol_whitelist", "pipe", "-readrate", "1", "-readrate_initial_burst", "0.001", "-threads", "1",
		"-filter_threads", "1", "-filter_complex_threads", "1",
		"-analyzeduration", "2000000", "-probesize", "2097152", "-f", "mpegts", "-i", "pipe:0",
		"-map", "0:v:0", "-map", "0:a:0", "-sn", "-dn",
		"-vf", "showinfo=checksum=0", "-af", "ashowinfo", "-fps_mode", "passthrough",
		"-c:v", "wrapped_avframe", "-c:a", "pcm_s16le", "-f", "null", os.DevNull,
	}
	cmd := exec.CommandContext(ctx, decoder, args...)
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return report, errors.New("decoder input pipe failed")
	}
	defer stdin.Close()
	startUsage := selfUsage()
	start := time.Now()
	obs := newObserver(start)
	out, diagnostic := &lineWriter{line: obs.progress}, &lineWriter{line: obs.stderr}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	report = Report{StartedAt: start.UTC(), Scope: "one finite HLS transfer through Go into an FFmpeg software decoder paced at 1x; null audio/video sink; no presentation or platform analytics", StallMeasurement: "not_measured; decoded-frame log gaps are observations, not player buffer stalls", Decoder: DecoderReport{Version: version, BinarySHA256: identity, Arguments: args, ExitCode: -1}}
	if err = cmd.Start(); err != nil {
		report.StopReason = "decoder_start_failed"
		return report, errors.New("decoder process could not start")
	}
	// The context-bound decoder closes its pipe on cancellation. This unblocks
	// a source waiting on backpressure; every started process is then reaped.
	report.Stream, err = stream(ctx, mediaprobe.StreamConfig{URL: cfg.URL, Client: cfg.Client, Timeout: cfg.RequestTimeout, ProxyURL: cfg.ProxyURL, RootCAs: cfg.RootCAs}, stdin)
	streamErr := err
	stdin.Close()
	if streamErr != nil {
		cancel()
	}
	waitErr := cmd.Wait() // also joins exec's two bounded output-copy goroutines
	out.flush()
	diagnostic.flush()
	end, endUsage := time.Now(), selfUsage()
	report.EndedAt, report.ElapsedSeconds = end.UTC(), end.Sub(start).Seconds()
	if cmd.ProcessState != nil {
		report.Decoder.ExitCode = cmd.ProcessState.ExitCode()
	}
	report.Decoder.Stats = obs.snapshot()
	report.Coverage = Coverage{ManifestSeconds: report.Stream.MediaDurationSeconds, VideoSeconds: report.Decoder.Stats.DecodedVideoDurationSeconds, AudioSeconds: report.Decoder.Stats.AudioDurationSeconds, ToleranceSeconds: 0.25}
	if c := &report.Coverage; c.ManifestSeconds > 0 {
		c.VideoMatches = c.VideoSeconds != nil && math.Abs(*c.VideoSeconds-c.ManifestSeconds) <= c.ToleranceSeconds
		c.AudioMatches = math.Abs(c.AudioSeconds-c.ManifestSeconds) <= c.ToleranceSeconds
	}
	report.GoResource = resourceDelta(startUsage, endUsage, report.ElapsedSeconds)
	report.Decoder.Resource = resourceTotal(childUsage(cmd.ProcessState), report.ElapsedSeconds)
	if report.GoResource.CPUAvailable && report.Decoder.Resource.CPUAvailable {
		sum := report.GoResource.CPUPercentOneCore + report.Decoder.Resource.CPUPercentOneCore
		report.CombinedCPUPercentOneCore = &sum
	}
	if parent.Err() != nil {
		report.StopReason = "cancelled"
		if errors.Is(parent.Err(), context.DeadlineExceeded) {
			report.StopReason = "deadline"
		}
		return report, parent.Err()
	}
	if streamErr != nil {
		report.StopReason = "stream_failed"
		if errors.Is(streamErr, context.DeadlineExceeded) {
			report.StopReason = "deadline"
		}
		return report, streamErr
	}
	if ctx.Err() != nil {
		report.StopReason = "deadline"
		return report, ctx.Err()
	}
	if waitErr != nil {
		report.StopReason = "decoder_failed"
		return report, errors.New("decoder failed; inspect bounded decoder diagnostics")
	}
	s := report.Decoder.Stats
	if !s.ProgressEnd || s.VideoFrames <= 0 || s.AudioFrames <= 0 || s.AudioSamples <= 0 || s.DecodeError || s.ParseErrors != 0 || s.ProgressFrames != s.VideoFrames {
		report.StopReason = "decode_evidence_incomplete"
		return report, errors.New("complete video and audio decoding was not established")
	}
	if report.Stream.StopReason != "complete" || report.Stream.CompletedSegments != report.Stream.SegmentCount || report.Stream.BytesDownloaded != report.Stream.DeclaredBytes || report.Stream.BytesWritten != report.Stream.DeclaredBytes {
		report.StopReason = "transfer_evidence_incomplete"
		return report, errors.New("complete media transfer was not established")
	}
	if !report.Coverage.VideoMatches || !report.Coverage.AudioMatches {
		report.StopReason = "duration_mismatch"
		return report, errors.New("decoded video/audio duration does not cover the declared clip within 250ms tolerance")
	}
	report.Completed, report.StopReason = true, "complete"
	return report, nil
}

func decoderIdentity(ctx context.Context, path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", errors.New("decoder executable unreadable")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	f.Close()
	if err != nil || n > 256<<20 {
		return "", "", errors.New("decoder identity could not be recorded")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, "-version")
	var output limitedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if cmd.Run() != nil {
		return "", "", errors.New("decoder version check failed")
	}
	first, _, _ := strings.Cut(output.String(), "\n")
	if !strings.HasPrefix(first, "ffmpeg version ") {
		return "", "", errors.New("decoder did not identify as FFmpeg")
	}
	return hex.EncodeToString(h.Sum(nil)), first, nil
}

type limitedOutput struct{ bytes.Buffer }

func (w *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if w.Len() < 16<<10 {
		w.Buffer.Write(p[:min(n, (16<<10)-w.Len())])
	}
	return n, nil
}

// exec writes each output stream serially. Its copy goroutines are joined by
// Wait before flush; the observer synchronizes across stdout and stderr.
type lineWriter struct {
	pending []byte
	line    func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			i = len(p)
		}
		if len(w.pending)+i > 64<<10 {
			return 0, errors.New("decoder diagnostic line exceeds limit")
		}
		w.pending = append(w.pending, p[:i]...)
		if i == len(p) {
			break
		}
		w.line(strings.TrimSuffix(string(w.pending), "\r"))
		w.pending = w.pending[:0]
		p = p[i+1:]
	}
	return n, nil
}
func (w *lineWriter) flush() {
	if len(w.pending) > 0 {
		w.line(string(w.pending))
		w.pending = nil
	}
}

type usage struct {
	user, system float64
	rss          *int64
	available    bool
}

func resourceDelta(a, b usage, elapsed float64) Resource {
	r := resourceTotal(b, elapsed)
	r.CPUAvailable = a.available && b.available
	if r.CPUAvailable {
		r.UserSeconds = b.user - a.user
		r.SystemSeconds = b.system - a.system
		r.CPUPercentOneCore = 100 * (r.UserSeconds + r.SystemSeconds) / elapsed
	}
	return r
}
func resourceTotal(u usage, elapsed float64) Resource {
	r := Resource{CPUAvailable: u.available, UserSeconds: u.user, SystemSeconds: u.system, PeakRSSBytes: u.rss}
	if u.available && elapsed > 0 {
		r.CPUPercentOneCore = 100 * (u.user + u.system) / elapsed
	}
	return r
}
