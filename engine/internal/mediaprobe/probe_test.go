package mediaprobe

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return server, roots
}
func mp4() []byte {
	data := make([]byte, 24)
	binary.BigEndian.PutUint32(data, 24)
	copy(data[4:], "ftypisom")
	copy(data[16:], "isomiso2")
	return data
}
func single(url, client string) Config {
	return Config{URL: url, Client: client, Duration: 40 * time.Millisecond, Interval: time.Second, Timeout: time.Second}
}

func TestMP4BothProfilesRangeAndPrivacy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, client := range []string{"stdlib", "chrome_152"} {
		t.Run(client, func(t *testing.T) {
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-65535" || r.Header.Get("Cookie") != "" {
					t.Errorf("unexpected request headers: %v", r.Header)
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Set-Cookie", "private-cookie=value")
				w.Header().Set("Content-Range", "bytes 0-23/24")
				w.WriteHeader(206)
				w.Write(mp4())
			})
			report, err := probe(context.Background(), single(server.URL+"/video.mp4?token=private-query", client), roots)
			if err != nil || report.Kind != "mp4" || report.Successes != 1 || report.Failures != 0 || report.Samples[0].BytesRead != 24 {
				t.Fatalf("MP4: %v %+v", err, report)
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "?token") {
				t.Fatal("private data in report")
			}
		})
	}
}

func TestHLSBothProfilesSelectLowestBandwidthAndRelativeSegment(t *testing.T) {
	for _, client := range []string{"stdlib", "chrome_152"} {
		t.Run(client, func(t *testing.T) {
			var high, segments atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				switch r.URL.Path {
				case "/master.m3u8":
					fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=200000,CODECS=\"a,b\"\nhigh.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=100000\nhls/low.m3u8?secret=variant-token\n")
				case "/high.m3u8":
					high.Add(1)
				case "/hls/low.m3u8":
					fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\n../segment.ts?secret=segment-token\n#EXT-X-ENDLIST\n")
				case "/segment.ts":
					segments.Add(1)
					if r.Header.Get("Range") != "bytes=0-65535" {
						t.Errorf("segment range: %s", r.Header.Get("Range"))
					}
					w.Write(bytes.Repeat([]byte{0x47, 0, 0, 0}, 256))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			report, err := probe(context.Background(), single(server.URL+"/master.m3u8?secret=master-token", client), roots)
			if err != nil || report.Successes != 1 || report.Kind != "hls" || high.Load() != 0 || segments.Load() != 1 || len(report.Samples[0].Requests) != 3 {
				t.Fatalf("HLS: %v %+v", err, report)
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "-token") || strings.Contains(string(encoded), "secret=") {
				t.Fatal("manifest credentials retained")
			}
		})
	}
}

func TestHLSByteRangePrefixValidation(t *testing.T) {
	for _, mode := range []string{"valid", "ignored", "wrong-start", "early-resource-eof", "short-body"} {
		t.Run(mode, func(t *testing.T) {
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/playlist.m3u8" {
					fmt.Fprint(w, "#EXTM3U\n#EXT-X-BYTERANGE:745044@2976416\n#EXTINF:2,\n729.ts\n#EXT-X-ENDLIST\n")
					return
				}
				if r.Header.Get("Range") != "bytes=2976416-3041951" {
					t.Errorf("wrong byterange: %s", r.Header.Get("Range"))
				}
				if mode == "ignored" {
					w.Write([]byte("not the requested offset"))
					return
				}
				cr := "bytes 2976416-3041951/10000000"
				w.Header().Set("Content-Type", "application/octet-stream")
				if mode == "wrong-start" {
					cr = "bytes 0-65535/10000000"
				}
				if mode == "early-resource-eof" {
					cr = "bytes 2976416-2976425/2976426"
				}
				w.Header().Set("Content-Range", cr)
				w.WriteHeader(206)
				if mode == "short-body" {
					w.Write([]byte{0x47, 0, 0, 0})
					return
				}
				w.Write(bytes.Repeat([]byte{0x47}, prefixBytes))
			})
			report, err := probe(context.Background(), single(server.URL+"/playlist.m3u8", "stdlib"), roots)
			if mode == "valid" {
				if err != nil || report.Successes != 1 {
					t.Fatalf("valid range: %v %+v", err, report)
				}
				request := report.Samples[0].Requests[1]
				if request.RequestedRange != "bytes=2976416-3041951" || request.ContentRange != "bytes 2976416-3041951/10000000" {
					t.Fatalf("missing range evidence: %+v", request)
				}
			} else if err == nil || report.Failures != 1 || report.Successes != 0 {
				t.Fatalf("bad range accepted: %v %+v", err, report)
			}
		})
	}
}

func TestBodyAndRangeFailures(t *testing.T) {
	for _, tc := range []struct {
		name, class  string
		status       int
		contentRange string
		length       string
		body         []byte
	}{
		{"empty", "empty_body", 200, "", "", nil},
		{"short", "short_body", 200, "", "", []byte("short")},
		{"truncated", "truncated_body", 200, "", "100", mp4()},
		{"range", "range_mismatch", 206, "bytes 1-24/25", "", mp4()},
		{"range-length", "range_length_mismatch", 206, "bytes 0-23/24", "20", mp4()[:20]},
		{"missing-range", "range_mismatch", 206, "", "", mp4()},
		{"html", "page_url", 200, "", "", []byte("<!DOCTYPE html><html>private-token</html>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}
				w.WriteHeader(tc.status)
				w.Write(tc.body)
			})
			report, err := probe(context.Background(), single(server.URL+"/video.mp4", "stdlib"), roots)
			if err == nil || report.Failures != 1 || report.Samples[0].ErrorClass != tc.class {
				t.Fatalf("body classification: %v %+v", err, report)
			}
		})
	}
}

func TestRestrictedStopsAndRetainsSafeRetryAfter(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				fmt.Fprint(w, "private-response-token")
			})
			cfg := single(server.URL+"/video.mp4?secret=hidden", "stdlib")
			cfg.Duration = 2 * time.Second
			report, err := probe(context.Background(), cfg, roots)
			if err == nil || report.StopReason != "restricted" || calls.Load() != 1 || report.Samples[0].Requests[0].RetryAfter != "2m0s" || report.Samples[0].BytesRead != 0 {
				t.Fatalf("restriction: %v %+v", err, report)
			}
		})
	}
}

func TestRedirectPolicy(t *testing.T) {
	for _, mode := range []string{"safe", "downgrade", "credentials", "loop"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/final.mp4" {
					w.Write(mp4())
					return
				}
				target := "/final.mp4?token=secret"
				if mode == "downgrade" {
					target = "http://127.0.0.1:1/private-token"
				}
				if mode == "credentials" {
					target = "https://name:private-token@127.0.0.1/"
				}
				if mode == "loop" {
					target = "/start.mp4"
				}
				w.Header().Set("Location", target)
				w.WriteHeader(302)
			})
			report, err := probe(context.Background(), single(server.URL+"/start.mp4", "stdlib"), roots)
			if mode == "safe" {
				if err != nil || report.Successes != 1 || calls.Load() != 2 {
					t.Fatalf("safe redirect: %v %+v", err, report)
				}
			} else if err == nil || report.Samples[0].ErrorClass != "redirect_policy" || calls.Load() > 4 {
				t.Fatalf("unsafe redirect: %v %+v", err, report)
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded)+fmt.Sprint(err), "private-token") || strings.Contains(string(encoded), "token=secret") {
				t.Fatal("redirect secrets in report")
			}
		})
	}
}

func TestDurationInterruptIsInconclusiveAndParentCancelIsPreserved(t *testing.T) {
	for _, client := range []string{"stdlib", "chrome_152"} {
		t.Run(client, func(t *testing.T) {
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			report, err := probe(context.Background(), single(server.URL+"/video.mp4", client), roots)
			if err == nil || report.StopReason != "inconclusive" || report.Interrupted != 1 || report.Failures != 0 {
				t.Fatalf("deadline became outage or success: %v %+v", err, report)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			report, err = probe(ctx, single(server.URL+"/video.mp4", client), roots)
			if !errors.Is(err, context.Canceled) || report.Failures != 0 || report.StopReason != "cancelled" {
				t.Fatalf("parent cancellation: %v %+v", err, report)
			}
		})
	}
	if _, err := Probe(nil, single("https://example.test/video.mp4", "stdlib")); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestRepeatedSamplesReuseClientConnection(t *testing.T) {
	var newConnections, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Range", "bytes 0-23/24")
		w.Header().Set("Content-Length", "24")
		w.WriteHeader(206)
		w.Write(mp4())
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	cfg := single(server.URL+"/video.mp4", "stdlib")
	cfg.Duration = 1150 * time.Millisecond
	report, err := probe(context.Background(), cfg, roots)
	if err != nil || report.Successes != 2 || requests.Load() != 2 || newConnections.Load() != 1 {
		t.Fatalf("pool reuse: %v %+v conns=%d", err, report, newConnections.Load())
	}
}

func TestHLSUnsupportedAndBounds(t *testing.T) {
	for _, tc := range []struct{ body, class string }{
		{"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key-secret\"\n#EXTINF:2,\nseg.ts\n", "unsupported_encryption"},
		{"#EXTM3U\n#EXT-X-PART:DURATION=1,URI=\"part.ts\"\n", "unsupported_llhls"},
		{"#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n", "invalid_manifest"},
		{"#EXTM3U\n#EXTINF:2,\n#EXT-X-BYTERANGE:100\nseg.ts\n", "unsupported_implicit_byterange"},
		{"#EXTM3U\n#EXTINF:2,\nhttp://example.test/seg.ts\n", "unsupported_url"},
		{"#EXTM3U\n" + strings.Repeat("#padding\n", manifestBytes/8), "manifest_too_large"},
	} {
		t.Run(tc.class, func(t *testing.T) {
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
			report, err := probe(context.Background(), single(server.URL+"/playlist.m3u8", "stdlib"), roots)
			if err == nil || report.Samples[0].ErrorClass != tc.class || len(report.Samples[0].Requests) != 1 {
				t.Fatalf("unsupported HLS: %v %+v", err, report)
			}
		})
	}
	if ValidateConfig(Config{URL: "https://example.test/a.mp4", Duration: 5 * time.Minute, Interval: time.Second, Timeout: time.Second}) == nil {
		t.Fatal("sample budget not enforced")
	}
	if ValidateConfig(Config{URL: "https://example.test/a.mp4", Duration: time.Second, Interval: 500 * time.Millisecond, Timeout: time.Second}) == nil {
		t.Fatal("subsecond interval accepted")
	}
}

func TestHLSDoesNotAcceptJSONErrorAsSegment(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/playlist.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:2,\nsegment.ts\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":"private-expired-token"}`)
	})
	report, err := probe(context.Background(), single(server.URL+"/playlist.m3u8", "stdlib"), roots)
	if err == nil || report.Successes != 0 || report.Samples[0].ErrorClass != "non_media_body" {
		t.Fatalf("JSON error treated as media: %v %+v", err, report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded)+fmt.Sprint(err), "private-expired-token") {
		t.Fatal("body leaked")
	}
}

func TestRequestTimeoutCountsFailureBeforeProbeDuration(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	cfg := single(server.URL+"/video.mp4", "stdlib")
	cfg.Duration, cfg.Timeout = 150*time.Millisecond, 20*time.Millisecond
	report, err := probe(context.Background(), cfg, roots)
	if err == nil || report.Failures != 1 || report.Interrupted != 0 || report.Samples[0].ErrorClass != "timeout" {
		t.Fatalf("sample timeout suppressed: %v %+v", err, report)
	}
}

func TestPartialResponseWaitsForHTTP2EndStream(t *testing.T) {
	for _, profile := range []string{"stdlib", "chrome_152"} {
		t.Run(profile, func(t *testing.T) {
			var peerCloses, writeErrors, written, completed, h2Requests atomic.Int64
			payload := bytes.Repeat([]byte{0x47}, prefixBytes)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer completed.Add(1)
				if r.ProtoMajor == 2 {
					h2Requests.Add(1)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", fmt.Sprint(prefixBytes))
				w.Header().Set("Content-Range", "bytes 2976416-3041951/3721460")
				w.WriteHeader(http.StatusPartialContent)
				n, err := w.Write(payload)
				written.Add(int64(n))
				if err != nil {
					writeErrors.Add(1)
					return
				}
				if err := http.NewResponseController(w).Flush(); err != nil {
					writeErrors.Add(1)
					return
				}
				// The complete payload is available, but END_STREAM is emitted
				// only after the handler returns. Closing at the exact byte cap
				// cancels this still-open stream instead of completing it.
				select {
				case <-r.Context().Done():
					peerCloses.Add(1)
				case <-time.After(15 * time.Millisecond):
				}
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			fetch, closeClient, err := newFetcher(Config{Client: profile, Timeout: time.Second}, roots)
			if err != nil {
				t.Fatal(err)
			}
			defer closeClient()
			target, _ := mediaURL(server.URL + "/segment.ts")
			const requests = 16
			var read int64
			for i := 0; i < requests; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				result := fetch(ctx, target, false, &byteRange{2976416, 745044})
				cancel()
				if result.class != "" || result.request.Status != 206 || len(result.body) != prefixBytes {
					t.Fatalf("range response %d: class=%s status=%d bytes=%d", i, result.class, result.request.Status, len(result.body))
				}
				read += int64(result.request.BytesRead)
			}
			deadline := time.Now().Add(time.Second)
			for completed.Load() != requests && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if completed.Load() != requests || h2Requests.Load() != requests || peerCloses.Load() != 0 || writeErrors.Load() != 0 || read != written.Load() {
				t.Fatalf("HTTP2 partial response audit: complete=%d h2=%d peer_closes=%d write_errors=%d server_bytes=%d client_bytes=%d", completed.Load(), h2Requests.Load(), peerCloses.Load(), writeErrors.Load(), written.Load(), read)
			}
		})
	}
}

func TestPartialResponseRejectsExtraDataAndUnfinishedStream(t *testing.T) {
	for _, mode := range []string{"extra", "unfinished"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Range", "bytes 0-65535/65536")
				w.WriteHeader(http.StatusPartialContent)
				// Flush headers to keep Content-Length unknown for the extra-byte
				// case, so the media probe owns the range-length validation.
				http.NewResponseController(w).Flush()
				w.Write(bytes.Repeat([]byte{0x47}, prefixBytes))
				if mode == "extra" {
					w.Write([]byte{0x47})
					return
				}
				http.NewResponseController(w).Flush()
				<-r.Context().Done()
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			fetch, closeClient, err := newFetcher(Config{Client: "chrome_152", Timeout: time.Second}, roots)
			if err != nil {
				t.Fatal(err)
			}
			defer closeClient()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			target, _ := mediaURL(server.URL + "/segment.ts")
			result := fetch(ctx, target, false, nil)
			want := "range_length_mismatch"
			if mode == "unfinished" {
				want = "timeout"
			}
			if result.class != want {
				t.Fatalf("%s response accepted or misclassified: class=%q bytes=%d", mode, result.class, result.request.BytesRead)
			}
		})
	}
}

func TestFullResponseStillStopsAtPrefixWithoutWaitingForEOF(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		w.Write(bytes.Repeat([]byte{0x47}, prefixBytes))
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	fetch, closeClient, err := newFetcher(Config{Client: "chrome_152", Timeout: time.Second}, roots)
	if err != nil {
		t.Fatal(err)
	}
	defer closeClient()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	target, _ := mediaURL(server.URL + "/full-file")
	result := fetch(ctx, target, false, nil)
	if result.class != "" || result.request.Status != 200 || result.request.BytesRead != prefixBytes {
		t.Fatalf("200 prefix should not drain the complete object: class=%q status=%d bytes=%d", result.class, result.request.Status, result.request.BytesRead)
	}
}
