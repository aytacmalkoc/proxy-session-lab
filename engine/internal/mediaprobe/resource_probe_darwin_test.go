package mediaprobe

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type mediaResourceConfig struct {
	ReadyPath string `json:"ready_path"`
	Profile   string `json:"profile"`
	Clients   int    `json:"clients"`
	Duration  string `json:"duration"`
	Interval  string `json:"interval"`
	Output    string `json:"output"`
}

type mediaResourceReady struct {
	URL            string `json:"url"`
	CertificatePEM string `json:"cert_pem"`
	ManifestBytes  int    `json:"manifest_bytes"`
	PrefixBytes    int    `json:"prefix_bytes"`
	RequestedRange string `json:"requested_range"`
	ContentRange   string `json:"content_range"`
	HTTP2Enabled   bool   `json:"http2_enabled"`
}

type mediaResourceUsage struct {
	user, system float64
	rss          int64
}

func mediaResourceUsageNow() mediaResourceUsage {
	var value syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &value); err != nil {
		panic(err)
	}
	return mediaResourceUsage{float64(value.Utime.Sec) + float64(value.Utime.Usec)/1e6, float64(value.Stime.Sec) + float64(value.Stime.Usec)/1e6, value.Maxrss}
}

func mediaResourceFDs() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

type mediaResourceSample struct {
	AfterMS         int64   `json:"after_ms"`
	HeapBytes       uint64  `json:"heap_bytes"`
	TotalAllocBytes uint64  `json:"total_alloc_bytes"`
	Goroutines      int     `json:"goroutines"`
	FDs             int     `json:"fds"`
	PeakRSSBytes    int64   `json:"process_peak_rss_bytes"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	ActiveClients   int64   `json:"active_clients"`
}

type mediaResourceClientResult struct {
	ID        int       `json:"id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	ElapsedMS float64   `json:"elapsed_ms"`
	Error     string    `json:"error,omitempty"`
	Report    Report    `json:"report"`
}

// TestMediaResourceProbe is opt-in and runs real media-probe clients against the
// companion loopback fixture only. Compile the test binary WITHOUT -race for
// resource measurements; each measured case must use a fresh process.
func TestMediaResourceProbe(t *testing.T) {
	if os.Getenv("KICKPOP_MEDIA_RESOURCE_PROBE") != "1" {
		t.Skip("opt-in isolated media resource measurement")
	}
	data, err := os.ReadFile(os.Getenv("KICKPOP_MEDIA_RESOURCE_CONFIG"))
	if err != nil {
		t.Fatal("cannot read media resource configuration")
	}
	var cfg mediaResourceConfig
	if json.Unmarshal(data, &cfg) != nil {
		t.Fatal("invalid media resource configuration")
	}
	duration, err := time.ParseDuration(cfg.Duration)
	if err != nil || duration < time.Second || duration > 5*time.Minute {
		t.Fatal("duration must be 1s..5m")
	}
	interval, err := time.ParseDuration(cfg.Interval)
	if err != nil || interval < time.Second {
		t.Fatal("interval must be at least 1s")
	}
	if cfg.Clients < 1 || cfg.Clients > 100 || (cfg.Profile != "stdlib" && cfg.Profile != "chrome_152") {
		t.Fatal("unsupported client count/profile")
	}
	data, err = os.ReadFile(cfg.ReadyPath)
	if err != nil {
		t.Fatal("cannot read fixture readiness file")
	}
	var ready mediaResourceReady
	if json.Unmarshal(data, &ready) != nil {
		t.Fatal("invalid fixture readiness file")
	}
	u, err := url.Parse(ready.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || !strings.HasSuffix(u.Path, ".m3u8") {
		t.Fatal("media resource fixture must be an HTTPS 127.0.0.1 playlist with an explicit port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid fixture port")
	}
	if ready.PrefixBytes != prefixBytes || ready.ManifestBytes <= 0 || ready.ManifestBytes > manifestBytes || ready.RequestedRange != "bytes=2976416-3041951" || ready.ContentRange != "bytes 2976416-3041951/3721460" {
		t.Fatal("unexpected synthetic fixture byte-range contract")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(ready.CertificatePEM)) {
		t.Fatal("invalid fixture certificate")
	}
	probeConfig := Config{URL: ready.URL, Client: cfg.Profile, Duration: duration, Interval: interval, Timeout: 10 * time.Second}
	if err = ValidateConfig(probeConfig); err != nil {
		t.Fatal(err)
	}
	// Reserve the report before requests and never replace earlier evidence.
	output, err := os.OpenFile(cfg.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("cannot create a new media resource report")
	}
	defer output.Close()

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	baselineGoroutines, baselineFDs := runtime.NumGoroutine(), mediaResourceFDs()
	var fdLimit syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &fdLimit)
	startUsage := mediaResourceUsageNow()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), duration+15*time.Second)
	defer cancel()
	clientResults := make([]mediaResourceClientResult, cfg.Clients)
	resourceSamples := make([]mediaResourceSample, 0, int(duration/(250*time.Millisecond))+8)
	var active, peak atomic.Int64
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
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				usage := mediaResourceUsageNow()
				resourceSamples = append(resourceSamples, mediaResourceSample{AfterMS: time.Since(start).Milliseconds(), HeapBytes: stats.HeapAlloc, TotalAllocBytes: stats.TotalAlloc, Goroutines: runtime.NumGoroutine(), FDs: mediaResourceFDs(), PeakRSSBytes: usage.rss, CPUSeconds: usage.user + usage.system - startUsage.user - startUsage.system, ActiveClients: active.Load()})
			}
		}
	}()
	barrier := make(chan struct{})
	var prepared, workers sync.WaitGroup
	prepared.Add(cfg.Clients)
	workers.Add(cfg.Clients)
	for index := range clientResults {
		go func(index int) {
			defer workers.Done()
			prepared.Done()
			<-barrier
			clientStart := time.Now()
			current := active.Add(1)
			for previous := peak.Load(); current > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, current) {
					break
				}
			}
			report, probeErr, panicked := mediaResourceCall(ctx, probeConfig, roots)
			active.Add(-1)
			clientEnd := time.Now()
			class := ""
			switch {
			case panicked:
				class = "probe_panic"
			case errors.Is(probeErr, context.Canceled):
				class = "cancelled"
			case errors.Is(probeErr, context.DeadlineExceeded):
				class = "deadline_exceeded"
			case probeErr != nil:
				class = "probe_failed"
			}
			clientResults[index] = mediaResourceClientResult{ID: index + 1, StartedAt: clientStart.UTC(), EndedAt: clientEnd.UTC(), ElapsedMS: float64(clientEnd.Sub(clientStart)) / float64(time.Millisecond), Error: class, Report: report}
		}(index)
	}
	prepared.Wait()
	barrierReleased := time.Now()
	close(barrier)
	workers.Wait()
	cancel()
	close(stopSampler)
	<-joinedSampler
	end := time.Now()
	endUsage := mediaResourceUsageNow()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	goroutinesAfter, fdsAfter := runtime.NumGoroutine(), mediaResourceFDs()
	// Deliberately outside CPU/elapsed: post-GC heap is a cleanup indicator,
	// not a claim that the OS returned the process RSS to its baseline.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var postGC runtime.MemStats
	runtime.ReadMemStats(&postGC)
	goroutinesSettled, fdsSettled := runtime.NumGoroutine(), mediaResourceFDs()

	successes, failures, interrupted, completedClients, failedClients := 0, 0, 0, 0, 0
	var bodyBytes int64
	statusCounts := make(map[string]int)
	rangeCounts := map[string]int{"requested": 0, "valid_206": 0, "invalid": 0, "ignored_200": 0}
	var latencies []float64
	var firstStart, lastStart time.Time
	minClientDuration, maxClientDuration := math.Inf(1), float64(0)
	clientStartsMS := make([]float64, len(clientResults))
	contractErrors := 0
	for i, client := range clientResults {
		if client.Error == "" && client.Report.StopReason == "duration" && client.Report.Successes > 0 && client.Report.Failures == 0 && client.Report.Interrupted == 0 {
			completedClients++
		} else {
			failedClients++
		}
		if firstStart.IsZero() || client.StartedAt.Before(firstStart) {
			firstStart = client.StartedAt
		}
		if lastStart.IsZero() || client.StartedAt.After(lastStart) {
			lastStart = client.StartedAt
		}
		clientStartsMS[i] = float64(client.StartedAt.Sub(barrierReleased.UTC())) / float64(time.Millisecond)
		minClientDuration = min(minClientDuration, client.ElapsedMS)
		maxClientDuration = max(maxClientDuration, client.ElapsedMS)
		successes += client.Report.Successes
		failures += client.Report.Failures
		interrupted += client.Report.Interrupted
		for _, sample := range client.Report.Samples {
			latencies = append(latencies, sample.ElapsedMS)
			bodyBytes += int64(sample.BytesRead)
			if sample.OK && (sample.Kind != "hls" || len(sample.Requests) != 2 || sample.BytesRead != ready.ManifestBytes+ready.PrefixBytes) {
				contractErrors++
			}
			for _, request := range sample.Requests {
				statusCounts[strconv.Itoa(request.Status)]++
				requestURL, parseErr := url.Parse(request.URL)
				if parseErr != nil || requestURL.Scheme != "https" || requestURL.Host != u.Host {
					contractErrors++
				}
				if request.RequestedRange != "" {
					rangeCounts["requested"]++
					if request.Status == 206 && request.RequestedRange == ready.RequestedRange && request.ContentRange == ready.ContentRange && request.BytesRead == ready.PrefixBytes {
						rangeCounts["valid_206"]++
					} else {
						rangeCounts["invalid"]++
					}
					if request.Status == 200 {
						rangeCounts["ignored_200"]++
					}
				}
			}
		}
	}
	peakHeap := max(before.HeapAlloc, after.HeapAlloc)
	peakGoroutines, peakFDs := max(baselineGoroutines, goroutinesAfter), max(baselineFDs, fdsAfter)
	for _, sample := range resourceSamples {
		peakHeap = max(peakHeap, sample.HeapBytes)
		peakGoroutines = max(peakGoroutines, sample.Goroutines)
		peakFDs = max(peakFDs, sample.FDs)
	}
	elapsed := end.Sub(start).Seconds()
	wall := end.UTC().Sub(start.UTC()).Seconds()
	cpuUser, cpuSystem := endUsage.user-startUsage.user, endUsage.system-startUsage.system
	result := map[string]any{
		"method":  "isolated non-race Go test process; real media-probe HTTP/TLS/HLS/BYTERANGE clients; barrier start; separate synthetic fixture; no external media",
		"scope":   "bounded manifest and segment-prefix access; no decode/playback; body byte counts exclude HTTP/TLS overhead; post-GC cleanup and JSON output excluded from CPU window",
		"profile": cfg.Profile, "clients": cfg.Clients, "duration": cfg.Duration, "interval": cfg.Interval, "go_version": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "fd_soft_limit": fdLimit.Cur,
		"started_at": start.UTC(), "ended_at": end.UTC(), "elapsed_seconds": elapsed, "wall_seconds": wall, "cpu_user_seconds": cpuUser, "cpu_system_seconds": cpuSystem, "cpu_percent_one_core": 100 * (cpuUser + cpuSystem) / elapsed,
		"peak_rss_bytes": endUsage.rss, "heap_before_bytes": before.HeapAlloc, "sampled_peak_heap_bytes": peakHeap, "heap_after_bytes": after.HeapAlloc, "heap_after_gc_bytes": postGC.HeapAlloc, "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "gc_cycles": after.NumGC - before.NumGC, "gc_pause_ns": after.PauseTotalNs - before.PauseTotalNs,
		"goroutines_before": baselineGoroutines, "sampled_peak_goroutines": peakGoroutines, "goroutines_after": goroutinesAfter, "goroutines_settled": goroutinesSettled, "fds_before": baselineFDs, "sampled_peak_fds": peakFDs, "fds_after": fdsAfter, "fds_settled": fdsSettled,
		"completed_clients": completedClients, "failed_clients": failedClients, "ended_clients": len(clientResults), "peak_concurrent_clients": peak.Load(), "active_clients_at_end": active.Load(), "successes": successes, "failures": failures, "interrupted": interrupted, "total_body_bytes": bodyBytes, "status_counts": statusCounts, "range_response_counts": rangeCounts, "fixture_contract_errors": contractErrors,
		"latency_ms": mediaResourceLatency(latencies), "latency_sample_count": len(latencies), "start_spread_ms": float64(lastStart.Sub(firstStart)) / float64(time.Millisecond), "client_start_delay_ms": clientStartsMS, "client_duration_ms": map[string]float64{"min": minClientDuration, "max": maxClientDuration},
		"samples": resourceSamples, "client_results": clientResults,
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(result); err != nil {
		t.Fatal("cannot write media resource report")
	}
	if err = output.Close(); err != nil {
		t.Fatal("cannot close media resource report")
	}
	t.Logf("profile=%s clients=%d completed=%d peak=%d RSS=%.2fMiB CPU=%.2f%% samples=%d/%d/%d bytes=%d goroutines=%d->%d FD=%d->%d", cfg.Profile, cfg.Clients, completedClients, peak.Load(), float64(endUsage.rss)/(1<<20), 100*(cpuUser+cpuSystem)/elapsed, successes, failures, interrupted, bodyBytes, baselineGoroutines, goroutinesSettled, baselineFDs, fdsSettled)
	if completedClients != cfg.Clients || failedClients != 0 || failures != 0 || interrupted != 0 || peak.Load() != int64(cfg.Clients) || active.Load() != 0 || contractErrors != 0 || rangeCounts["invalid"] != 0 || rangeCounts["valid_206"] != successes {
		t.Error("incomplete or inconsistent media resource measurement; inspect report")
	}
	if elapsed > duration.Seconds()+5 {
		t.Error("cleanup exceeded 5s")
	}
	if delta := wall - elapsed; delta > 0.1 || delta < -0.1 {
		t.Error("clock divergence invalidates measurement")
	}
}

func mediaResourceCall(ctx context.Context, cfg Config, roots *x509.CertPool) (report Report, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	report, err = probe(ctx, cfg, roots)
	return report, err, false
}

// Percentiles use nearest rank. Samples include unsuccessful/aborted samples so
// a slow failure is not hidden by an otherwise fast successful subset.
func mediaResourceLatency(values []float64) map[string]float64 {
	result := map[string]float64{"mean": 0, "p50": 0, "p95": 0, "max": 0}
	if len(values) == 0 {
		return result
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	var sum float64
	for _, value := range ordered {
		sum += value
	}
	result["mean"] = sum / float64(len(ordered))
	result["p50"] = ordered[int(math.Ceil(float64(len(ordered))*.50))-1]
	result["p95"] = ordered[int(math.Ceil(float64(len(ordered))*.95))-1]
	result["max"] = ordered[len(ordered)-1]
	return result
}
