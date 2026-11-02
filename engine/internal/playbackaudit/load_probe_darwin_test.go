package playbackaudit

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/mediaprobe"
)

type playbackLoadConfig struct {
	ReadyPath            string   `json:"ready_path"`
	Profile              string   `json:"profile"`
	Clients              int      `json:"clients"`
	Timeout              string   `json:"timeout"`
	Manifests            []string `json:"manifests"`
	ExpectedMediaSeconds float64  `json:"expected_media_seconds"`
	ExpectedVideoFrames  int      `json:"expected_video_frames"`
	ExpectedAudioSamples int64    `json:"expected_audio_samples"`
	Decoder              string   `json:"decoder"`
	Output               string   `json:"output"`
}

type playbackLoadProbeReady struct {
	BaseURL        string `json:"base_url"`
	CertificatePEM string `json:"cert_pem"`
}

type playbackLoadSample struct {
	AfterMS         int64   `json:"after_ms"`
	HeapBytes       uint64  `json:"heap_bytes"`
	TotalAllocBytes uint64  `json:"total_alloc_bytes"`
	NumGC           uint32  `json:"num_gc"`
	Goroutines      int     `json:"goroutines"`
	FDs             int     `json:"fds"`
	PeakRSSBytes    *int64  `json:"go_process_peak_rss_bytes,omitempty"`
	CPUSeconds      float64 `json:"go_cpu_seconds"`
	ActiveClients   int64   `json:"active_clients"`
}

type playbackLoadClient struct {
	ID             int                       `json:"id"`
	StartedAt      time.Time                 `json:"started_at"`
	EndedAt        time.Time                 `json:"ended_at"`
	StartAfterMS   float64                   `json:"start_after_barrier_ms"`
	ElapsedSeconds float64                   `json:"elapsed_seconds"`
	RealTimeFactor float64                   `json:"real_time_factor"`
	Error          string                    `json:"error,omitempty"`
	Windows        []mediaprobe.StreamReport `json:"windows"`
	Report         Report                    `json:"report"`
}

type playbackLoadMemory struct {
	HeapBeforeBytes       uint64 `json:"heap_before_bytes"`
	HeapAfterBytes        uint64 `json:"heap_after_bytes"`
	HeapPeakSampledBytes  uint64 `json:"heap_peak_sampled_bytes"`
	HeapSettledBytes      uint64 `json:"heap_settled_bytes"`
	TotalAllocatedBytes   uint64 `json:"total_allocated_bytes"`
	GCs                   uint32 `json:"gcs"`
	GCPauseNanoseconds    uint64 `json:"gc_pause_nanoseconds"`
	GoroutinesBefore      int    `json:"goroutines_before"`
	GoroutinesAfter       int    `json:"goroutines_after"`
	GoroutinesPeakSampled int    `json:"goroutines_peak_sampled"`
	GoroutinesSettled     int    `json:"goroutines_settled"`
	FDsBefore             int    `json:"fds_before"`
	FDsAfter              int    `json:"fds_after"`
	FDsPeakSampled        int    `json:"fds_peak_sampled"`
	FDsSettled            int    `json:"fds_settled"`
}

type playbackLoadResult struct {
	Scope                     string               `json:"scope"`
	MeasurementNotes          []string             `json:"measurement_notes"`
	Profile                   string               `json:"profile"`
	Clients                   int                  `json:"clients"`
	ExpectedMediaSeconds      float64              `json:"expected_media_seconds"`
	ExpectedVideoFrames       int                  `json:"expected_video_frames_per_client"`
	ExpectedAudioSamples      int64                `json:"expected_audio_samples_per_client"`
	Timeout                   string               `json:"timeout"`
	StartedAt                 time.Time            `json:"started_at"`
	EndedAt                   time.Time            `json:"ended_at"`
	BarrierReleasedAt         time.Time            `json:"barrier_released_at"`
	ElapsedSeconds            float64              `json:"elapsed_seconds"`
	StartSpreadMS             float64              `json:"start_spread_ms"`
	DecoderStartSpreadMS      float64              `json:"decoder_start_spread_ms"`
	CompletedClients          int                  `json:"completed_clients"`
	FailedClients             int                  `json:"failed_clients"`
	PeakConcurrentClients     int64                `json:"peak_concurrent_clients"`
	GoResource                Resource             `json:"go_resource"`
	DecoderResourcesAvailable int                  `json:"decoder_resources_available"`
	DecoderUserSeconds        float64              `json:"decoder_user_seconds"`
	DecoderSystemSeconds      float64              `json:"decoder_system_seconds"`
	DecoderCPUPercentOneCore  float64              `json:"decoder_cpu_percent_one_core"`
	CombinedCPUPercentOneCore float64              `json:"combined_cpu_percent_one_core"`
	TotalVideoFrames          int64                `json:"total_video_frames"`
	TotalAudioSamples         int64                `json:"total_audio_samples"`
	MediaBodyBytes            int64                `json:"media_body_bytes"`
	ManifestBodyBytes         int64                `json:"manifest_body_bytes"`
	MinClientElapsedSeconds   float64              `json:"min_client_elapsed_seconds"`
	MaxClientElapsedSeconds   float64              `json:"max_client_elapsed_seconds"`
	MinRealTimeFactor         float64              `json:"min_real_time_factor"`
	MaxRealTimeFactor         float64              `json:"max_real_time_factor"`
	Memory                    playbackLoadMemory   `json:"memory"`
	GOMAXPROCS                int                  `json:"gomaxprocs"`
	LogicalCPUs               int                  `json:"logical_cpus"`
	FDLimit                   uint64               `json:"fd_limit"`
	Samples                   []playbackLoadSample `json:"samples"`
	Workers                   []playbackLoadClient `json:"workers"`
}

func playbackLoadFDs() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

// TestPlaybackLoadProbe is deliberately opt-in. Run each case in a fresh,
// non-race test process; its only permitted HTTP origin is the loopback fixture.
func TestPlaybackLoadProbe(t *testing.T) {
	if os.Getenv("KICKPOP_PLAYBACK_LOAD_PROBE") != "1" {
		t.Skip("opt-in isolated playback load measurement")
	}
	data, err := os.ReadFile(os.Getenv("KICKPOP_PLAYBACK_LOAD_CONFIG"))
	if err != nil {
		t.Fatal("cannot read playback load configuration")
	}
	var cfg playbackLoadConfig
	if json.Unmarshal(data, &cfg) != nil {
		t.Fatal("invalid playback load configuration")
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout < time.Second || timeout > 5*time.Minute || cfg.Clients < 1 || cfg.Clients > 100 {
		t.Fatal("timeout must be 1s..5m and clients must be 1..100")
	}
	if cfg.Profile != "stdlib" && cfg.Profile != "chrome_152" {
		t.Fatal("profile must be stdlib or chrome_152")
	}
	if math.IsNaN(cfg.ExpectedMediaSeconds) || math.IsInf(cfg.ExpectedMediaSeconds, 0) || cfg.ExpectedMediaSeconds <= 0 || cfg.ExpectedMediaSeconds > 181 || cfg.ExpectedMediaSeconds > timeout.Seconds() || cfg.ExpectedVideoFrames <= 0 || cfg.ExpectedVideoFrames > 1_000_000 || cfg.ExpectedAudioSamples <= 0 || cfg.ExpectedAudioSamples > 1_000_000_000 {
		t.Fatal("invalid expected media duration, video count, or audio count")
	}
	if len(cfg.Manifests) < 1 || len(cfg.Manifests) > 8 {
		t.Fatal("one to eight finite manifest windows are required")
	}
	data, err = os.ReadFile(cfg.ReadyPath)
	if err != nil {
		t.Fatal("cannot read playback fixture readiness")
	}
	var ready playbackLoadProbeReady
	if json.Unmarshal(data, &ready) != nil {
		t.Fatal("invalid playback fixture readiness")
	}
	base, err := url.Parse(ready.BaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() != "127.0.0.1" || base.Port() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" || (base.Path != "" && base.Path != "/") {
		t.Fatal("playback fixture must be an HTTPS 127.0.0.1 origin with an explicit port")
	}
	port, err := strconv.Atoi(base.Port())
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid fixture port")
	}
	base.Path = "/"
	urls := make([]string, len(cfg.Manifests))
	for i, name := range cfg.Manifests {
		if !strings.HasSuffix(name, ".m3u8") || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" || strings.Contains(name, "..") {
			t.Fatal("manifest names must be simple local .m3u8 filenames")
		}
		urls[i] = base.ResolveReference(&url.URL{Path: name}).String()
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ready.CertificatePEM)) {
		t.Fatal("invalid playback fixture certificate")
	}
	workerConfig := Config{URL: urls[0], Client: cfg.Profile, Decoder: cfg.Decoder, Timeout: timeout, RequestTimeout: 10 * time.Second}
	// Reuse production validation except for this test's separately bounded 5m
	// timeout. Public Run keeps its 2m limit unchanged.
	validationConfig := workerConfig
	validationConfig.Timeout = min(timeout, 2*time.Minute)
	if err = ValidateConfig(validationConfig); err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(cfg.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("cannot reserve a new playback load report")
	}
	defer output.Close()
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	results := make([]playbackLoadClient, cfg.Clients)
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	baselineGoroutines, baselineFDs := runtime.NumGoroutine(), playbackLoadFDs()
	var fdLimit syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &fdLimit)
	var active, peak atomic.Int64
	var prepared, workers sync.WaitGroup
	prepared.Add(cfg.Clients)
	workers.Add(cfg.Clients)
	barrier := make(chan struct{})
	var barrierReleased time.Time
	for index := range results {
		go func(index int) {
			defer workers.Done()
			prepared.Done()
			<-barrier
			start := time.Now()
			current := active.Add(1)
			for previous := peak.Load(); current > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, current) {
					break
				}
			}
			defer active.Add(-1)
			var windows []mediaprobe.StreamReport
			stream := playbackLoadStream(urls, roots, &windows)
			report, runErr := runValidated(ctx, workerConfig, stream)
			end := time.Now()
			class := ""
			switch {
			case errors.Is(runErr, context.Canceled):
				class = "cancelled"
			case errors.Is(runErr, context.DeadlineExceeded):
				class = "deadline_exceeded"
			case runErr != nil:
				class = "playback_failed"
			case !report.Completed || len(windows) != len(urls) || report.Decoder.Stats.VideoFrames != cfg.ExpectedVideoFrames || report.Decoder.Stats.AudioSamples != cfg.ExpectedAudioSamples || math.Abs(report.Stream.MediaDurationSeconds-cfg.ExpectedMediaSeconds) > 0.001:
				class = "fixture_evidence_mismatch"
			}
			elapsed := end.Sub(start).Seconds()
			results[index] = playbackLoadClient{ID: index + 1, StartedAt: start.UTC(), EndedAt: end.UTC(), StartAfterMS: float64(start.Sub(barrierReleased)) / float64(time.Millisecond), ElapsedSeconds: elapsed, RealTimeFactor: cfg.ExpectedMediaSeconds / elapsed, Error: class, Windows: windows, Report: report}
		}(index)
	}
	prepared.Wait()
	startUsage := selfUsage()
	if !startUsage.available {
		// No worker may remain behind an unopened barrier after a setup failure.
		stopSignals()
		barrierReleased = time.Now()
		close(barrier)
		workers.Wait()
		t.Fatal("process CPU accounting unavailable")
	}
	start := time.Now()
	samples := make([]playbackLoadSample, 0, int(timeout/(250*time.Millisecond))+8)
	stopSampler, joinedSampler := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joinedSampler)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-ticker.C:
				var memory runtime.MemStats
				runtime.ReadMemStats(&memory)
				u := selfUsage()
				samples = append(samples, playbackLoadSample{AfterMS: time.Since(start).Milliseconds(), HeapBytes: memory.HeapAlloc, TotalAllocBytes: memory.TotalAlloc, NumGC: memory.NumGC, Goroutines: runtime.NumGoroutine(), FDs: playbackLoadFDs(), PeakRSSBytes: u.rss, CPUSeconds: u.user + u.system - startUsage.user - startUsage.system, ActiveClients: active.Load()})
			}
		}
	}()
	barrierReleased = time.Now()
	close(barrier)
	workers.Wait()
	close(stopSampler)
	<-joinedSampler
	end := time.Now()
	endUsage := selfUsage()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	goroutinesAfter, fdsAfter := runtime.NumGoroutine(), playbackLoadFDs()
	// Cleanup sampling and JSON serialization are outside the CPU wall window.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var settled runtime.MemStats
	runtime.ReadMemStats(&settled)
	result := playbackLoadResult{
		Scope: "local finite HLS windows decoded sequentially into one paced software FFmpeg process per client; null sink; no platform traffic or engagement",
		MeasurementNotes: []string{
			"Cohort Go CPU is one RUSAGE_SELF delta; overlapping per-worker Report.GoResource and CombinedCPUPercentOneCore values are not attributable and are not summed.",
			"Decoder CPU sums each playback child's process-state user/system time and divides by the same cohort wall window as Go CPU; brief FFmpeg version-check subprocess CPU is not included.",
			"macOS Maxrss is process-lifetime peak bytes. Decoder peaks are retained individually and are not summed into a simultaneous cohort RSS. External process sampling supplies that measurement.",
			"250ms samples include sampler overhead. Post-GC cleanup and JSON encoding are outside the CPU window; heap includes retained worker evidence.",
			"Real-time factor is expected media seconds divided by worker wall seconds, including decoder identity/version startup. Frame-log arrival gaps are not presentation stalls.",
		},
		Profile: cfg.Profile, Clients: cfg.Clients, ExpectedMediaSeconds: cfg.ExpectedMediaSeconds, ExpectedVideoFrames: cfg.ExpectedVideoFrames, ExpectedAudioSamples: cfg.ExpectedAudioSamples, Timeout: timeout.String(),
		StartedAt: start.UTC(), EndedAt: end.UTC(), BarrierReleasedAt: barrierReleased.UTC(), ElapsedSeconds: end.Sub(start).Seconds(), PeakConcurrentClients: peak.Load(), GoResource: resourceDelta(startUsage, endUsage, end.Sub(start).Seconds()),
		MinClientElapsedSeconds: math.Inf(1), MinRealTimeFactor: math.Inf(1), GOMAXPROCS: runtime.GOMAXPROCS(0), LogicalCPUs: runtime.NumCPU(), FDLimit: fdLimit.Cur, Samples: samples, Workers: results,
		Memory: playbackLoadMemory{HeapBeforeBytes: before.HeapAlloc, HeapAfterBytes: after.HeapAlloc, HeapPeakSampledBytes: max(before.HeapAlloc, after.HeapAlloc), HeapSettledBytes: settled.HeapAlloc, TotalAllocatedBytes: after.TotalAlloc - before.TotalAlloc, GCs: after.NumGC - before.NumGC, GCPauseNanoseconds: after.PauseTotalNs - before.PauseTotalNs, GoroutinesBefore: baselineGoroutines, GoroutinesAfter: goroutinesAfter, GoroutinesPeakSampled: max(baselineGoroutines, goroutinesAfter), GoroutinesSettled: runtime.NumGoroutine(), FDsBefore: baselineFDs, FDsAfter: fdsAfter, FDsPeakSampled: max(baselineFDs, fdsAfter), FDsSettled: playbackLoadFDs()},
	}
	var firstStart, lastStart, firstDecoder, lastDecoder time.Time
	for _, client := range results {
		if client.Error == "" && client.Report.Completed {
			result.CompletedClients++
		} else {
			result.FailedClients++
		}
		if firstStart.IsZero() || client.StartedAt.Before(firstStart) {
			firstStart = client.StartedAt
		}
		if lastStart.IsZero() || client.StartedAt.After(lastStart) {
			lastStart = client.StartedAt
		}
		if !client.Report.StartedAt.IsZero() {
			if firstDecoder.IsZero() || client.Report.StartedAt.Before(firstDecoder) {
				firstDecoder = client.Report.StartedAt
			}
			if lastDecoder.IsZero() || client.Report.StartedAt.After(lastDecoder) {
				lastDecoder = client.Report.StartedAt
			}
		}
		resource := client.Report.Decoder.Resource
		if resource.CPUAvailable {
			result.DecoderResourcesAvailable++
			result.DecoderUserSeconds += resource.UserSeconds
			result.DecoderSystemSeconds += resource.SystemSeconds
		}
		result.TotalVideoFrames += int64(client.Report.Decoder.Stats.VideoFrames)
		result.TotalAudioSamples += client.Report.Decoder.Stats.AudioSamples
		result.MediaBodyBytes += client.Report.Stream.BytesDownloaded
		for _, window := range client.Windows {
			result.ManifestBodyBytes += int64(window.Manifest.BytesRead)
		}
		result.MinClientElapsedSeconds = min(result.MinClientElapsedSeconds, client.ElapsedSeconds)
		result.MaxClientElapsedSeconds = max(result.MaxClientElapsedSeconds, client.ElapsedSeconds)
		result.MinRealTimeFactor = min(result.MinRealTimeFactor, client.RealTimeFactor)
		result.MaxRealTimeFactor = max(result.MaxRealTimeFactor, client.RealTimeFactor)
	}
	for _, sample := range samples {
		result.Memory.HeapPeakSampledBytes = max(result.Memory.HeapPeakSampledBytes, sample.HeapBytes)
		result.Memory.GoroutinesPeakSampled = max(result.Memory.GoroutinesPeakSampled, sample.Goroutines)
		result.Memory.FDsPeakSampled = max(result.Memory.FDsPeakSampled, sample.FDs)
	}
	result.StartSpreadMS = float64(lastStart.Sub(firstStart)) / float64(time.Millisecond)
	if !firstDecoder.IsZero() {
		result.DecoderStartSpreadMS = float64(lastDecoder.Sub(firstDecoder)) / float64(time.Millisecond)
	}
	result.DecoderCPUPercentOneCore = 100 * (result.DecoderUserSeconds + result.DecoderSystemSeconds) / result.ElapsedSeconds
	result.CombinedCPUPercentOneCore = result.GoResource.CPUPercentOneCore + result.DecoderCPUPercentOneCore
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		t.Fatal("cannot write playback load report")
	}
	if err := output.Sync(); err != nil {
		t.Fatal("cannot persist playback load report")
	}
	t.Logf("clients=%d completed=%d failed=%d wall=%.3fs video_frames=%d audio_samples=%d combined_cpu=%.2f%%", cfg.Clients, result.CompletedClients, result.FailedClients, result.ElapsedSeconds, result.TotalVideoFrames, result.TotalAudioSamples, result.CombinedCPUPercentOneCore)
	if result.FailedClients != 0 || result.CompletedClients != cfg.Clients || result.DecoderResourcesAvailable != cfg.Clients || !result.GoResource.CPUAvailable {
		t.Fatal("playback load case failed; complete per-client evidence was saved")
	}
}

func playbackLoadStream(urls []string, roots *x509.CertPool, windows *[]mediaprobe.StreamReport) streamer {
	return func(ctx context.Context, cfg mediaprobe.StreamConfig, sink io.Writer) (aggregate mediaprobe.StreamReport, err error) {
		start := time.Now()
		aggregate = mediaprobe.StreamReport{URL: urls[0], Client: cfg.Client, StartedAt: start.UTC(), EndList: true, Scope: "sequential finite loopback HLS windows; one decoder stdin"}
		defer func() {
			aggregate.EndedAt = time.Now().UTC()
			aggregate.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
		}()
		for _, target := range urls {
			cfg.URL, cfg.RootCAs = target, roots
			windowOffsetMS := float64(time.Since(start)) / float64(time.Millisecond)
			window, streamErr := mediaprobe.StreamVOD(ctx, cfg, sink)
			*windows = append(*windows, window)
			if len(*windows) == 1 {
				aggregate.Manifest = window.Manifest
			}
			for _, segment := range window.Segments {
				segment.Index += aggregate.SegmentCount
				segment.StartSeconds += aggregate.MediaDurationSeconds
				segment.CumulativeBytes += aggregate.BytesDownloaded
				segment.DownloadStartedMS += windowOffsetMS
				if segment.SinkWriteStartedMS > 0 {
					segment.SinkWriteStartedMS += windowOffsetMS
				}
				aggregate.Segments = append(aggregate.Segments, segment)
			}
			aggregate.SegmentCount += window.SegmentCount
			aggregate.CompletedSegments += window.CompletedSegments
			aggregate.MediaDurationSeconds += window.MediaDurationSeconds
			aggregate.DeclaredBytes += window.DeclaredBytes
			aggregate.BytesDownloaded += window.BytesDownloaded
			aggregate.BytesWritten += window.BytesWritten
			aggregate.EndList = aggregate.EndList && window.EndList
			if streamErr != nil {
				aggregate.StopReason = window.StopReason
				return aggregate, streamErr
			}
			if window.StopReason != "complete" || !window.EndList || window.CompletedSegments != window.SegmentCount {
				aggregate.StopReason = "window_incomplete"
				return aggregate, errors.New("finite media window did not complete")
			}
		}
		aggregate.StopReason = "complete"
		return aggregate, nil
	}
}
