package playbackaudit

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/mediaprobe"
)

type playbackLoadReady struct {
	BaseURL      string `json:"base_url"`
	CertPEM      string `json:"cert_pem"`
	PID          int    `json:"pid"`
	StatsPath    string `json:"stats_path"`
	HTTP2Enabled bool   `json:"http2_enabled"`
}
type playbackLoadStats struct {
	At                 time.Time `json:"at"`
	StartedAt          time.Time `json:"started_at"`
	Stopped            bool      `json:"stopped"`
	Requests           int64     `json:"requests"`
	Manifests          int64     `json:"manifests"`
	Segments           int64     `json:"segments"`
	BodyBytes          int64     `json:"body_bytes"`
	ActiveRequests     int64     `json:"active_requests"`
	PeakActiveRequests int64     `json:"peak_active_requests"`
	OpenConnections    int64     `json:"open_connections"`
	TotalConnections   int64     `json:"total_connections"`
	ClosedConnections  int64     `json:"closed_connections"`
	PeakConnections    int64     `json:"peak_connections"`
	Errors             int64     `json:"errors"`
	RangeErrors        int64     `json:"range_errors"`
	PeerCloses         int64     `json:"peer_closes"`
	HTTP1Requests      int64     `json:"http1_requests"`
	HTTP2Requests      int64     `json:"http2_requests"`
}
type playbackLoadFixture struct {
	root                                          *os.Root
	started                                       time.Time
	mu                                            sync.Mutex
	conns                                         map[net.Conn]struct{}
	requests, manifests, segments, bodyBytes      atomic.Int64
	active, peakActive, open, total, closed, peak atomic.Int64
	errors, rangeErrors, peerCloses, http1, http2 atomic.Int64
}

func newPlaybackLoadServer(directory string) (*httptest.Server, *playbackLoadFixture, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	fixture := &playbackLoadFixture{root: root, started: time.Now().UTC(), conns: make(map[net.Conn]struct{})}
	server := &httptest.Server{Listener: listener, EnableHTTP2: true, Config: &http.Server{
		Handler: http.HandlerFunc(fixture.handle), ConnState: fixture.connectionState,
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
	}}
	server.StartTLS()
	return server, fixture, nil
}

// TestPlaybackLoadServer only serves caller-prepared local media. It never
// fetches external media, resolves a Kick page, redirects, or opens a decoder.
func TestPlaybackLoadServer(t *testing.T) {
	if os.Getenv("KICKPOP_PLAYBACK_LOAD_SERVER") != "1" {
		t.Skip("opt in with KICKPOP_PLAYBACK_LOAD_SERVER=1")
	}
	directory, readyPath := os.Getenv("KICKPOP_PLAYBACK_LOAD_DIR"), os.Getenv("KICKPOP_PLAYBACK_LOAD_READY")
	if directory == "" || readyPath == "" {
		t.Fatal("KICKPOP_PLAYBACK_LOAD_DIR and KICKPOP_PLAYBACK_LOAD_READY are required")
	}
	statsPath := os.Getenv("KICKPOP_PLAYBACK_LOAD_STATS")
	if statsPath == "" {
		statsPath = readyPath + ".stats.json"
	}
	if filepath.Clean(readyPath) == filepath.Clean(statsPath) {
		t.Fatal("ready and stats paths must differ")
	}
	duration := 20 * time.Minute
	if raw := os.Getenv("KICKPOP_PLAYBACK_LOAD_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil || duration <= 0 || duration > 20*time.Minute {
			t.Fatal("server duration must be positive and at most 20m")
		}
	}
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, duration)
	defer cancel()
	server, fixture, err := newPlaybackLoadServer(directory)
	if err != nil {
		t.Fatal("cannot start local playback load fixture")
	}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := server.Config.Shutdown(shutdownCtx); err != nil {
			t.Errorf("local fixture HTTP shutdown: %v", err)
		}
		server.Close()
		deadline := time.Now().Add(time.Second)
		for (fixture.active.Load() != 0 || fixture.open.Load() != 0) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		fixture.root.Close()
		final := fixture.snapshot(true)
		if final.ActiveRequests != 0 || final.OpenConnections != 0 || final.TotalConnections != final.ClosedConnections {
			t.Errorf("fixture resources remain: %+v", final)
		}
		if err := writePlaybackLoadJSON(statsPath, final); err != nil {
			t.Errorf("write final fixture stats: %v", err)
		}
	}()
	ready := playbackLoadReady{BaseURL: server.URL, CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})), PID: os.Getpid(), StatsPath: statsPath, HTTP2Enabled: true}
	if err := writePlaybackLoadJSON(statsPath, fixture.snapshot(false)); err != nil {
		t.Fatal(err)
	}
	if err := writePlaybackLoadJSON(readyPath, ready); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writePlaybackLoadJSON(statsPath, fixture.snapshot(false)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func (s *playbackLoadFixture) handle(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	loadMaximum(&s.peakActive, s.active.Add(1))
	defer s.active.Add(-1)
	if r.ProtoMajor == 2 {
		s.http2.Add(1)
	} else {
		s.http1.Add(1)
	}
	fail := func(status int) { s.errors.Add(1); w.WriteHeader(status) }
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || r.Method != http.MethodGet {
		fail(http.StatusForbidden)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Path != "/"+name || name == "" || name == "." || path.Clean(name) != name || strings.Contains(name, "\\") || strings.HasPrefix(name, "../") || name == ".." {
		fail(http.StatusBadRequest)
		return
	}
	ext := path.Ext(name)
	if ext != ".m3u8" && ext != ".ts" {
		fail(http.StatusNotFound)
		return
	}
	// Root confines both path resolution and symlink traversal to the chosen
	// directory. Lstat additionally rejects final symlinks and non-regular files.
	info, err := s.root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		fail(http.StatusNotFound)
		return
	}
	file, err := s.root.Open(name)
	if err != nil {
		fail(http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		fail(http.StatusNotFound)
		return
	}
	start, length, status := int64(0), info.Size(), http.StatusOK
	if ext == ".m3u8" {
		s.manifests.Add(1)
		if r.Header.Get("Range") != "" {
			s.rangeErrors.Add(1)
			fail(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	} else {
		s.segments.Add(1)
		var end int64
		start, end, err = playbackClosedRange(r.Header.Get("Range"), info.Size())
		if err != nil {
			s.rangeErrors.Add(1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size()))
			fail(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		length, status = end-start+1, http.StatusPartialContent
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, info.Size()))
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	n, err := io.CopyN(w, io.NewSectionReader(file, start, length), length)
	s.bodyBytes.Add(n)
	if err != nil {
		if r.Context().Err() != nil || playbackPeerClosed(err) {
			s.peerCloses.Add(1)
		} else {
			s.errors.Add(1)
		}
	}
}

func playbackClosedRange(header string, size int64) (int64, int64, error) {
	invalid := errors.New("one closed byte interval is required")
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, invalid
	}
	left, right, ok := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if !ok || left == "" || right == "" || len(header) > 64 || strings.Trim(left+right, "0123456789") != "" {
		return 0, 0, invalid
	}
	start, a := strconv.ParseInt(left, 10, 64)
	end, b := strconv.ParseInt(right, 10, 64)
	if a != nil || b != nil || start < 0 || end < start || end >= size {
		return 0, 0, invalid
	}
	return start, end, nil
}
func (s *playbackLoadFixture) connectionState(conn net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case http.StateNew:
		if _, ok := s.conns[conn]; !ok {
			s.conns[conn] = struct{}{}
			s.total.Add(1)
			loadMaximum(&s.peak, s.open.Add(1))
		}
	case http.StateClosed, http.StateHijacked:
		if _, ok := s.conns[conn]; ok {
			delete(s.conns, conn)
			s.open.Add(-1)
			s.closed.Add(1)
		}
	}
}
func (s *playbackLoadFixture) snapshot(stopped bool) playbackLoadStats {
	return playbackLoadStats{At: time.Now().UTC(), StartedAt: s.started, Stopped: stopped, Requests: s.requests.Load(), Manifests: s.manifests.Load(), Segments: s.segments.Load(), BodyBytes: s.bodyBytes.Load(), ActiveRequests: s.active.Load(), PeakActiveRequests: s.peakActive.Load(), OpenConnections: s.open.Load(), TotalConnections: s.total.Load(), ClosedConnections: s.closed.Load(), PeakConnections: s.peak.Load(), Errors: s.errors.Load(), RangeErrors: s.rangeErrors.Load(), PeerCloses: s.peerCloses.Load(), HTTP1Requests: s.http1.Load(), HTTP2Requests: s.http2.Load()}
}
func loadMaximum(counter *atomic.Int64, candidate int64) {
	for old := counter.Load(); candidate > old && !counter.CompareAndSwap(old, candidate); old = counter.Load() {
	}
}
func playbackPeerClosed(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || err.Error() == "http2: stream closed" || err.Error() == "http2: client disconnected"
}
func writePlaybackLoadJSON(target string, value any) error {
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".playback-load-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), target)
}

func TestPlaybackLoadFixtureFilesAndPublicTrustSeam(t *testing.T) {
	directory := t.TempDir()
	data := make([]byte, 376)
	data[0], data[188] = 0x47, 0x47
	manifest := "#EXTM3U\n#EXT-X-BYTERANGE:376@0\n#EXTINF:2,\nmedia.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(filepath.Join(directory, "media.ts"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "clip.m3u8"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	server, fixture, err := newPlaybackLoadServer(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.root.Close()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	report, err := mediaprobe.StreamVOD(context.Background(), mediaprobe.StreamConfig{URL: server.URL + "/clip.m3u8", Client: "chrome_152", Timeout: time.Second, RootCAs: roots}, io.Discard)
	if err != nil || report.BytesWritten != 376 || report.CompletedSegments != 1 {
		t.Fatalf("public explicit-root stream: %v %+v", err, report)
	}
	stats := fixture.snapshot(false)
	if stats.Requests != 2 || stats.Manifests != 1 || stats.Segments != 1 || stats.BodyBytes != int64(376+len(manifest)) || stats.Errors != 0 || stats.HTTP2Requests != 2 {
		t.Fatalf("fixture transfer counters: %+v", stats)
	}
	serialized, _ := json.Marshal(mediaprobe.StreamConfig{RootCAs: roots})
	if strings.Contains(string(serialized), "RootCAs") {
		t.Fatal("trust pool serialized")
	}
	outside := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "escape.ts")); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		method, route, rangeHeader string
		status                     int
	}{
		{"GET", "/clip.m3u8?query=forbidden", "", 400}, {"GET", "/%2e%2e/outside.ts", "bytes=0-1", 400}, {"GET", "/escape.ts", "bytes=0-1", 404},
		{"POST", "/clip.m3u8", "", 403}, {"GET", "/media.ts", "bytes=0-", 416}, {"GET", "/media.ts", "bytes=0-400", 416}, {"GET", "/media.ts", "bytes=0-1,3-4", 416},
	} {
		req, _ := http.NewRequest(request.method, server.URL+request.route, nil)
		req.Header.Set("Range", request.rangeHeader)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != request.status {
			t.Fatalf("route %s got %d want %d", request.route, res.StatusCode, request.status)
		}
	}
	server.Client().CloseIdleConnections()
	server.Close()
	deadline := time.Now().Add(time.Second)
	for fixture.open.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	final := fixture.snapshot(true)
	if final.OpenConnections != 0 || final.ActiveRequests != 0 || final.TotalConnections != final.ClosedConnections {
		t.Fatalf("fixture resources: %+v", final)
	}
}
