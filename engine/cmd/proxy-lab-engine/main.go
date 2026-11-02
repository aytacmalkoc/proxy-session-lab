// proxy-lab-engine is the local NDJSON worker used by the Python CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/mediaprobe"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/playbackaudit"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/proxycheck"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/proxytransport"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/wstransport"
)

const version = "0.3.0"

type Job struct {
	Control string `json:"control,omitempty"`
	proxycheck.Config
	Test          string  `json:"test"`
	Decoder       string  `json:"decoder,omitempty"`
	MediaDuration float64 `json:"media_duration_seconds,omitempty"`
}

func perform(ctx context.Context, job Job) proxycheck.Report {
	if job.Test == "" || job.Test == "http" {
		r := proxycheck.Check(ctx, job.Config)
		r.Test = "http"
		return r
	}
	r := proxycheck.Report{ID: job.ID, Proxy: proxytransport.Redact(job.Proxy), Profile: job.Profile, Test: job.Test, Stages: []proxytransport.Stage{}}
	start := time.Now()
	fail := func(code string) proxycheck.Report {
		r.ErrorCode = code
		r.FailedStage = job.Test
		r.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
		return r
	}
	limit := float64(60)
	if job.Test == "decode" {
		limit = 120
	}
	if job.TimeoutSeconds <= 0 || job.TimeoutSeconds > limit || math.IsNaN(job.TimeoutSeconds) || math.IsInf(job.TimeoutSeconds, 0) {
		return fail("invalid_timeout")
	}
	if _, err := proxytransport.Parse(job.Proxy); err != nil {
		return fail("invalid_proxy")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(job.TimeoutSeconds*float64(time.Second)))
	defer cancel()
	switch job.Test {
	case "ws":
		conn, meta, err := wstransport.Dial(ctx, job.URL, wstransport.Config{Client: job.Profile, Timeout: time.Duration(job.TimeoutSeconds * float64(time.Second)), ProxyURL: job.Proxy})
		r.Details = struct {
			Scope     string               `json:"scope"`
			Transport wstransport.Metadata `json:"transport"`
		}{"verified_wss_upgrade_only", meta}
		r.HTTPStatus = meta.Status
		if err != nil {
			return fail("wss_upgrade_failed")
		}
		conn.Close()
		r.TransportOK = true
		r.TargetOK = true
	case "media":
		duration := job.MediaDuration
		if duration == 0 {
			duration = 5
		}
		if duration < 1 || duration > 60 {
			return fail("invalid_media_duration")
		}
		rep, err := mediaprobe.Probe(ctx, mediaprobe.Config{URL: job.URL, Client: job.Profile, ProxyURL: job.Proxy, Duration: time.Duration(duration * float64(time.Second)), Interval: time.Second, Timeout: time.Duration(min(job.TimeoutSeconds, 30) * float64(time.Second))})
		r.Details = rep
		for _, sample := range rep.Samples {
			if sample.Status != 0 {
				r.HTTPStatus = sample.Status
			}
			for _, req := range sample.Requests {
				if req.Status == 429 || req.Status == 503 {
					r.StopRun = true
				}
			}
		}
		if err != nil || rep.Failures > 0 || rep.Successes == 0 {
			return fail("media_access_failed")
		}
		r.TransportOK = true
		r.TargetOK = true
	case "decode":
		rep, err := playbackaudit.Run(ctx, playbackaudit.Config{URL: job.URL, Client: job.Profile, ProxyURL: job.Proxy, Decoder: job.Decoder, Timeout: time.Duration(job.TimeoutSeconds * float64(time.Second)), RequestTimeout: time.Duration(min(job.TimeoutSeconds, 30) * float64(time.Second))})
		r.Details = rep
		r.HTTPStatus = rep.Stream.Manifest.Status
		r.StopRun = r.HTTPStatus == 429 || r.HTTPStatus == 503
		for _, part := range rep.Stream.Segments {
			if part.Status == 429 || part.Status == 503 {
				r.StopRun = true
			}
		}
		if err != nil || !rep.Completed {
			return fail("decode_failed")
		}
		r.TransportOK = true
		r.TargetOK = true
	default:
		return fail("unknown_test")
	}
	r.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
	return r
}
func serve(parent context.Context, in io.Reader, out io.Writer, workers int) error {
	if workers < 1 || workers > 100 {
		return fmt.Errorf("workers must be 1..100")
	}
	if err := json.NewEncoder(out).Encode(map[string]any{"event": "ready", "protocol": 1, "version": version, "tls_profiles": []string{"stdlib", "chrome_152"}, "tests": []string{"http", "ws", "media", "decode"}}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	jobs := make(chan Job, workers)
	readErr := make(chan error, 1)
	go func() {
		defer close(jobs)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		for scanner.Scan() {
			var job Job
			if err := json.Unmarshal(scanner.Bytes(), &job); err != nil {
				readErr <- fmt.Errorf("invalid NDJSON input")
				cancel()
				return
			}
			if job.Control == "shutdown" {
				cancel()
				readErr <- nil
				return
			}
			if job.ID == "" || len(job.ID) > 128 {
				readErr <- fmt.Errorf("request requires an id of at most 128 bytes")
				cancel()
				return
			}
			select {
			case jobs <- job:
			case <-ctx.Done():
				return
			}
		}
		readErr <- scanner.Err()
	}()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var writeErr error
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					r := perform(ctx, job)
					mu.Lock()
					if writeErr == nil {
						writeErr = json.NewEncoder(out).Encode(r)
						if writeErr != nil {
							cancel()
						}
					}
					mu.Unlock()
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-parent.Done():
		cancel()
		<-done
		return parent.Err()
	case <-done:
	}
	if writeErr != nil {
		return writeErr
	}
	select {
	case err := <-readErr:
		return err
	default:
		return ctx.Err()
	}
}
func run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintf(out, "proxy-lab-engine %s; protocol=1; TLS=stdlib,chrome_152; tests=http,ws,media,decode\n", version)
		return nil
	}
	if len(args) > 0 && args[0] == "serve" {
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		workers := f.Int("workers", 8, "bounded NDJSON workers (1..100)")
		if err := f.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		return serve(ctx, in, out, *workers)
	}
	f := flag.NewFlagSet("check", flag.ContinueOnError)
	endpoint := f.String("url", "", "HTTP(S), WSS or direct media target")
	p := f.String("proxy", "", "proxy URL; omitted explicitly selects direct access")
	profile := f.String("tls-profile", "stdlib", "stdlib or chrome_152")
	kind := f.String("test", "http", "http, ws, media or decode")
	timeout := f.Float64("timeout", 15, "total request timeout in seconds (<=60)")
	decoder := f.String("decoder", "ffmpeg", "local decoder for decode tests")
	expect := f.String("expect-text", "", "required marker in bounded HTTP body")
	if len(args) > 0 && args[0] == "check" {
		args = args[1:]
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *endpoint == "" {
		return fmt.Errorf("usage: proxy-lab-engine check --url URL [--proxy URL] [--tls-profile stdlib|chrome_152]; or serve/version")
	}
	report := perform(ctx, Job{Config: proxycheck.Config{ID: "check", URL: *endpoint, Proxy: *p, Profile: *profile, TimeoutSeconds: *timeout, ExpectText: *expect}, Test: *kind, Decoder: *decoder})
	if err := json.NewEncoder(out).Encode(report); err != nil {
		return err
	}
	if !report.TargetOK {
		return fmt.Errorf("check failed: %s", report.ErrorCode)
	}
	return nil
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "proxy-lab-engine:", err)
		os.Exit(1)
	}
}
