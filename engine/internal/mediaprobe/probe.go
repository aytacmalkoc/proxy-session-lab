// Package mediaprobe samples bounded media prefixes and transfers finite VOD
// segments. It does not decode or play media; callers own optional decoder sinks.
package mediaprobe

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/proxytransport"
	"net/url"
	"strings"
	"time"
)

const prefixBytes = 64 << 10
const manifestBytes = 256 << 10

type Config struct {
	ProxyURL                    string `json:"-"`
	URL                         string
	Client                      string
	Duration, Interval, Timeout time.Duration
}

type Request struct {
	Stage          string  `json:"stage"`
	URL            string  `json:"url"`
	Status         int     `json:"status"`
	ElapsedMS      float64 `json:"elapsed_ms"`
	BytesRead      int     `json:"bytes_read"`
	RetryAfter     string  `json:"retry_after,omitempty"`
	RequestedRange string  `json:"requested_range,omitempty"`
	ContentRange   string  `json:"content_range,omitempty"`
}
type Sample struct {
	At          time.Time `json:"at"`
	ElapsedMS   float64   `json:"elapsed_ms"`
	Kind        string    `json:"kind"`
	Status      int       `json:"status"`
	OK          bool      `json:"ok"`
	ErrorClass  string    `json:"error_class,omitempty"`
	BytesRead   int       `json:"bytes_read"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Requests    []Request `json:"requests"`
}
type Report struct {
	URL         string    `json:"url"`
	Client      string    `json:"client"`
	Kind        string    `json:"kind"`
	Scope       string    `json:"scope"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	ElapsedMS   float64   `json:"elapsed_ms"`
	Duration    string    `json:"duration"`
	Interval    string    `json:"interval"`
	Successes   int       `json:"successes"`
	Failures    int       `json:"failures"`
	Interrupted int       `json:"interrupted"`
	StopReason  string    `json:"stop_reason"`
	Samples     []Sample  `json:"samples"`
}

func defaults(cfg Config) Config {
	if cfg.Client == "" {
		cfg.Client = "stdlib"
	}
	if cfg.Duration == 0 {
		cfg.Duration = 30 * time.Second
	}
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return cfg
}

func ValidateConfig(cfg Config) error {
	cfg = defaults(cfg)
	if _, err := proxytransport.Parse(cfg.ProxyURL); err != nil {
		return err
	}
	if _, err := mediaURL(cfg.URL); err != nil {
		return err
	}
	if cfg.Client != "stdlib" && cfg.Client != "chrome_152" {
		return errors.New("client must be stdlib or chrome_152")
	}
	if cfg.Duration <= 0 || cfg.Duration > 5*time.Minute {
		return errors.New("duration must be positive and at most 5m")
	}
	if cfg.Interval < time.Second {
		return errors.New("interval must be at least 1s")
	}
	if cfg.Timeout <= 0 || cfg.Timeout > 30*time.Second {
		return errors.New("timeout must be positive and at most 30s")
	}
	if 1+(cfg.Duration-1)/cfg.Interval > 120 {
		return errors.New("configuration exceeds 120 samples")
	}
	return nil
}

func Probe(ctx context.Context, cfg Config) (Report, error) { return probe(ctx, cfg, nil) }

// roots is only for verified local TLS fixtures; public callers use system CAs.
func probe(parent context.Context, cfg Config, roots *x509.CertPool) (report Report, err error) {
	if parent == nil {
		return report, errors.New("context must not be nil")
	}
	cfg = defaults(cfg)
	if err := ValidateConfig(cfg); err != nil {
		return report, err
	}
	u, _ := mediaURL(cfg.URL)
	fetch, closeClient, err := newFetcher(cfg, roots)
	if err != nil {
		return report, errors.New("media client initialization failed")
	}
	defer closeClient()
	start := time.Now()
	report = Report{URL: redactedURL(u), Client: cfg.Client, Kind: "unknown", Scope: "prefix_access_only; no decoding, playback, complete-segment integrity, or HLS init-map validation", StartedAt: start.UTC(), Duration: cfg.Duration.String(), Interval: cfg.Interval.String()}
	defer func() {
		report.EndedAt = time.Now().UTC()
		report.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
	}()
	ctx, cancel := context.WithTimeout(parent, cfg.Duration)
	defer cancel()
	for len(report.Samples) < 120 {
		if ctx.Err() != nil {
			break
		}
		sampleCtx, sampleCancel := context.WithTimeout(ctx, cfg.Timeout)
		sample := sampleMedia(sampleCtx, u, fetch)
		sampleCancel()
		if ctx.Err() != nil && (sample.ErrorClass == "timeout" || sample.ErrorClass == "cancelled") {
			sample.Interrupted, sample.ErrorClass = true, "probe_deadline"
			if parent.Err() != nil {
				sample.ErrorClass = "cancelled"
			}
			report.Interrupted++
		} else if sample.OK {
			report.Successes++
		} else {
			report.Failures++
		}
		report.Samples = append(report.Samples, sample)
		if sample.Kind != "unknown" {
			report.Kind = sample.Kind
		}
		if sample.Interrupted {
			break
		}
		if sample.Status == 401 || sample.Status == 403 || sample.Status == 429 {
			report.StopReason = "restricted"
			return report, errors.New("media sampling stopped by HTTP access or rate limit")
		}
		if terminalClass(sample.ErrorClass) {
			report.StopReason = "unsupported"
			return report, errors.New("media sampling stopped: " + sample.ErrorClass)
		}
		timer := time.NewTimer(cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if parent.Err() != nil {
		report.StopReason = "cancelled"
		return report, parent.Err()
	}
	report.StopReason = "duration"
	if report.Failures > 0 {
		return report, errors.New("one or more media access samples failed")
	}
	if report.Successes == 0 {
		report.StopReason = "inconclusive"
		return report, errors.New("no media access sample completed successfully")
	}
	return report, nil
}

func terminalClass(class string) bool {
	return class == "page_url" || class == "unsupported_type" || class == "invalid_manifest" || class == "manifest_too_large" || class == "redirect_policy" || class == "unsupported_url" || strings.HasPrefix(class, "unsupported_")
}

func sampleMedia(ctx context.Context, u *url.URL, fetch fetcher) (sample Sample) {
	start := time.Now()
	sample.At, sample.Kind = start.UTC(), "unknown"
	defer func() { sample.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond) }()
	read := func(stage string, target *url.URL, full bool, selected *byteRange) responseData {
		value := fetch(ctx, target, full, selected)
		value.request.Stage = stage
		sample.Requests = append(sample.Requests, value.request)
		sample.BytesRead += value.request.BytesRead
		sample.Status = value.request.Status
		if value.class != "" {
			sample.ErrorClass = value.class
		}
		return value
	}
	isHLS := strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
	if isHLS {
		sample.Kind = "hls"
	} else if strings.HasSuffix(strings.ToLower(u.Path), ".mp4") {
		sample.Kind = "mp4"
	}
	first := read("media", u, isHLS, nil)
	if first.class != "" {
		return sample
	}
	if isHTML(first.body, first.contentType) {
		sample.ErrorClass = "page_url"
		return sample
	}
	isHLS = isHLS || bytes.HasPrefix(bytes.TrimSpace(first.body), []byte("#EXTM3U")) || strings.Contains(strings.ToLower(first.contentType), "mpegurl")
	if !isHLS {
		mimeType := strings.ToLower(strings.TrimSpace(strings.Split(first.contentType, ";")[0]))
		if mimeType == "video/mp4" || mimeType == "audio/mp4" || mimeType == "application/mp4" {
			sample.Kind = "mp4"
		}
		if len(first.body) >= 8 && string(first.body[4:8]) == "ftyp" {
			sample.Kind = "mp4"
		}
		if sample.Kind != "mp4" {
			sample.ErrorClass = "unsupported_type"
			return sample
		}
		sample.ErrorClass = verifyMP4(first.body)
		sample.OK = sample.ErrorClass == ""
		return sample
	}
	sample.Kind = "hls"
	if !strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
		// A ranged discovery response cannot prove that a playlist is complete.
		first = read("manifest", first.finalURL, true, nil)
		if first.class != "" {
			return sample
		}
	}
	playlist, class := parsePlaylist(first.body, first.finalURL)
	if class != "" {
		sample.ErrorClass = class
		return sample
	}
	if playlist.variant != nil {
		variant := read("variant", playlist.variant, true, nil)
		if variant.class != "" {
			return sample
		}
		playlist, class = parsePlaylist(variant.body, variant.finalURL)
		if class != "" {
			sample.ErrorClass = class
			return sample
		}
		if playlist.variant != nil {
			sample.ErrorClass = "unsupported_nested_master"
			return sample
		}
	}
	if playlist.segment == nil {
		sample.ErrorClass = "invalid_manifest"
		return sample
	}
	segment := read("segment", playlist.segment, false, playlist.segmentRange)
	if segment.class != "" {
		return sample
	}
	if isHTML(segment.body, segment.contentType) {
		sample.ErrorClass = "page_url"
		return sample
	}
	if nonMediaBody(segment.body, segment.contentType) {
		sample.ErrorClass = "non_media_body"
		return sample
	}
	sample.OK = true
	return sample
}

func verifyMP4(body []byte) string {
	if len(body) < 16 {
		return "short_body"
	}
	if string(body[4:8]) != "ftyp" {
		return "invalid_mp4"
	}
	size, minimum := uint64(binary.BigEndian.Uint32(body[:4])), uint64(16)
	if size == 1 {
		if len(body) < 24 {
			return "short_body"
		}
		size, minimum = binary.BigEndian.Uint64(body[8:16]), 24
	}
	if size < minimum || (size-minimum)%4 != 0 {
		return "invalid_mp4"
	}
	if size > uint64(len(body)) {
		return "short_body"
	}
	return ""
}

func mediaURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("media URL must be HTTPS without credentials or a fragment")
	}
	return u, nil
}
func redactedURL(u *url.URL) string {
	copy := *u
	copy.RawQuery, copy.Fragment, copy.RawFragment, copy.ForceQuery = "", "", "", false
	return copy.String()
}
func isHTML(body []byte, contentType string) bool {
	prefix := strings.ToLower(string(bytes.TrimSpace(body[:min(len(body), 256)])))
	return strings.Contains(strings.ToLower(contentType), "text/html") || strings.HasPrefix(prefix, "<!doctype html") || strings.HasPrefix(prefix, "<html")
}

func nonMediaBody(body []byte, contentType string) bool {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.Contains(mime, "json") || strings.Contains(mime, "xml") || strings.HasPrefix(mime, "text/") {
		return true
	}
	prefix := bytes.TrimSpace(body[:min(len(body), 256)])
	return bytes.HasPrefix(prefix, []byte("{")) || bytes.HasPrefix(prefix, []byte("[")) || bytes.HasPrefix(prefix, []byte("<?xml")) || bytes.HasPrefix(prefix, []byte("<Error"))
}
