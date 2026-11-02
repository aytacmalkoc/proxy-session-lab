package mediaprobe

import (
	"context"
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
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This literal preserves the observed 906-byte VOD playlist's structure without
// reading live artifacts or contacting a CDN. Segment contents below are synthetic.
const mediaFixtureManifest = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-TARGETDURATION:2
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:28.419Z
#EXT-X-BYTERANGE:745044@2976416
#EXTINF:2.000,
729.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:30.419Z
#EXT-X-BYTERANGE:740720@0
#EXTINF:2.000,
730.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:32.419Z
#EXT-X-BYTERANGE:742788@740720
#EXTINF:2.000,
730.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:34.419Z
#EXT-X-BYTERANGE:745796@1483508
#EXTINF:2.000,
730.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:36.419Z
#EXT-X-BYTERANGE:745420@2229304
#EXTINF:2.000,
730.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:38.419Z
#EXT-X-BYTERANGE:745420@2974724
#EXTINF:2.000,
730.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:40.419Z
#EXT-X-BYTERANGE:745984@0
#EXTINF:2.000,
731.ts
#EXT-X-PROGRAM-DATE-TIME:2026-10-18T20:29:42.419Z
#EXT-X-BYTERANGE:743352@745984
#EXTINF:2.000,
731.ts
#EXT-X-ENDLIST
`

const mediaFixtureRequestedRange = "bytes=2976416-3041951"
const mediaFixtureContentRange = "bytes 2976416-3041951/3721460"

type mediaFixtureReady struct {
	URL            string `json:"url"`
	CertPEM        string `json:"cert_pem"`
	StatsPath      string `json:"stats_path"`
	PID            int    `json:"pid"`
	ManifestBytes  int    `json:"manifest_bytes"`
	PrefixBytes    int    `json:"prefix_bytes"`
	RequestedRange string `json:"requested_range"`
	ContentRange   string `json:"content_range"`
	HTTP2Enabled   bool   `json:"http2_enabled"`
	SyntheticMedia bool   `json:"synthetic_media"`
}

type mediaFixtureStats struct {
	At                time.Time `json:"at"`
	StartedAt         time.Time `json:"started_at"`
	Stopped           bool      `json:"stopped"`
	Requests          int64     `json:"requests"`
	ManifestRequests  int64     `json:"manifest_requests"`
	SegmentRequests   int64     `json:"segment_requests"`
	BodyBytes         int64     `json:"body_bytes"`
	ActiveRequests    int64     `json:"active_requests"`
	PeakActive        int64     `json:"peak_active_requests"`
	OpenConnections   int64     `json:"open_connections"`
	PeakConnections   int64     `json:"peak_connections"`
	TotalConnections  int64     `json:"total_connections"`
	ClosedConnections int64     `json:"closed_connections"`
	RangeErrors       int64     `json:"range_errors"`
	HandlerErrors     int64     `json:"handler_errors"`
	PeerWriteCloses   int64     `json:"peer_write_closes"`
	HTTP1Requests     int64     `json:"http1_requests"`
	HTTP2Requests     int64     `json:"http2_requests"`
}

type mediaFixtureServer struct {
	started                                  time.Time
	prefix                                   []byte
	mu                                       sync.Mutex
	conns                                    map[net.Conn]struct{}
	requests, manifests, segments, bodyBytes atomic.Int64
	active, peakActive                       atomic.Int64
	open, peak, total, closed                atomic.Int64
	rangeErrors, handlerErrors, peerCloses   atomic.Int64
	http1, http2                             atomic.Int64
}

// TestMediaResourceServer runs only by explicit opt-in and listens on loopback.
// Use a test binary timeout longer than the chosen server duration (max 20m).
func TestMediaResourceServer(t *testing.T) {
	if os.Getenv("KICKPOP_MEDIA_RESOURCE_SERVER") != "1" {
		t.Skip("set KICKPOP_MEDIA_RESOURCE_SERVER=1 for the separate local fixture")
	}
	readyPath := os.Getenv("KICKPOP_MEDIA_RESOURCE_READY")
	if readyPath == "" {
		t.Fatal("KICKPOP_MEDIA_RESOURCE_READY is required")
	}
	statsPath := os.Getenv("KICKPOP_MEDIA_RESOURCE_STATS")
	if statsPath == "" {
		statsPath = readyPath + ".stats.json"
	}
	if filepath.Clean(readyPath) == filepath.Clean(statsPath) {
		t.Fatal("ready and stats paths must differ")
	}
	duration := 20 * time.Minute
	if raw := os.Getenv("KICKPOP_MEDIA_RESOURCE_DURATION"); raw != "" {
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
	fixture := &mediaFixtureServer{started: time.Now().UTC(), prefix: makeMediaFixturePrefix(), conns: make(map[net.Conn]struct{})}
	server := httptest.NewUnstartedServer(http.HandlerFunc(fixture.handle))
	server.EnableHTTP2 = true
	server.Config.ConnState = fixture.connectionState
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Config.WriteTimeout = 5 * time.Second
	server.Config.IdleTimeout = 15 * time.Second
	server.StartTLS()
	ready := mediaFixtureReady{
		URL:       server.URL + "/clip/playlist.m3u8",
		CertPEM:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})),
		StatsPath: statsPath, PID: os.Getpid(), ManifestBytes: len(mediaFixtureManifest), PrefixBytes: len(fixture.prefix),
		RequestedRange: mediaFixtureRequestedRange, ContentRange: mediaFixtureContentRange, HTTP2Enabled: true, SyntheticMedia: true,
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := server.Config.Shutdown(shutdownCtx); err != nil {
			t.Errorf("fixture HTTP shutdown: %v", err)
		}
		server.Close()
		// ConnState closure callbacks can follow Shutdown's idle close briefly.
		deadline := time.Now().Add(time.Second)
		for (fixture.open.Load() != 0 || fixture.active.Load() != 0) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		final := fixture.snapshot(true)
		if final.ActiveRequests != 0 || final.OpenConnections != 0 || final.TotalConnections != final.ClosedConnections {
			t.Errorf("fixture resources remain open: %+v", final)
		}
		if err := writeMediaFixtureJSON(statsPath, final); err != nil {
			t.Errorf("write final media fixture stats: %v", err)
		}
	}()
	if err := writeMediaFixtureJSON(statsPath, fixture.snapshot(false)); err != nil {
		t.Fatal(err)
	}
	if err := writeMediaFixtureJSON(readyPath, ready); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeMediaFixtureJSON(statsPath, fixture.snapshot(false)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func (s *mediaFixtureServer) handle(w http.ResponseWriter, r *http.Request) {
	s.requests.Add(1)
	active := s.active.Add(1)
	mediaFixtureMax(&s.peakActive, active)
	defer s.active.Add(-1)
	if r.ProtoMajor == 2 {
		s.http2.Add(1)
	} else {
		s.http1.Add(1)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || r.Method != http.MethodGet {
		s.handlerErrors.Add(1)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	var payload []byte
	status := http.StatusOK
	switch r.URL.Path {
	case "/clip/playlist.m3u8":
		s.manifests.Add(1)
		if r.Header.Get("Range") != "" {
			s.rangeErrors.Add(1)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		payload = []byte(mediaFixtureManifest)
	case "/clip/729.ts":
		s.segments.Add(1)
		if r.Header.Get("Range") != mediaFixtureRequestedRange {
			s.rangeErrors.Add(1)
			w.Header().Set("Content-Range", "bytes */3721460")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", mediaFixtureContentRange)
		payload, status = s.prefix, http.StatusPartialContent
	default:
		s.handlerErrors.Add(1)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	n, err := w.Write(payload)
	s.bodyBytes.Add(int64(n))
	if err != nil {
		if r.Context().Err() != nil || mediaFixturePeerClosed(err) {
			s.peerCloses.Add(1)
		} else {
			s.handlerErrors.Add(1)
		}
	}
}

func (s *mediaFixtureServer) connectionState(conn net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch state {
	case http.StateNew:
		if _, exists := s.conns[conn]; !exists {
			s.conns[conn] = struct{}{}
			s.total.Add(1)
			mediaFixtureMax(&s.peak, s.open.Add(1))
		}
	case http.StateClosed, http.StateHijacked:
		if _, exists := s.conns[conn]; exists {
			delete(s.conns, conn)
			s.open.Add(-1)
			s.closed.Add(1)
		}
	}
}

func (s *mediaFixtureServer) snapshot(stopped bool) mediaFixtureStats {
	return mediaFixtureStats{
		At: time.Now().UTC(), StartedAt: s.started, Stopped: stopped,
		Requests: s.requests.Load(), ManifestRequests: s.manifests.Load(), SegmentRequests: s.segments.Load(), BodyBytes: s.bodyBytes.Load(),
		ActiveRequests: s.active.Load(), PeakActive: s.peakActive.Load(), OpenConnections: s.open.Load(), PeakConnections: s.peak.Load(), TotalConnections: s.total.Load(), ClosedConnections: s.closed.Load(),
		RangeErrors: s.rangeErrors.Load(), HandlerErrors: s.handlerErrors.Load(), PeerWriteCloses: s.peerCloses.Load(), HTTP1Requests: s.http1.Load(), HTTP2Requests: s.http2.Load(),
	}
}

func mediaFixtureMax(value *atomic.Int64, candidate int64) {
	for previous := value.Load(); candidate > previous && !value.CompareAndSwap(previous, candidate); previous = value.Load() {
	}
}

// These are null TS packets with a sync byte every 188 bytes. They carry no
// video or audio program and only model transfer cost for the prefix probe.
func makeMediaFixturePrefix() []byte {
	data := make([]byte, prefixBytes)
	for i := range data {
		data[i] = 0xff
	}
	for offset := 0; offset+4 <= len(data); offset += 188 {
		copy(data[offset:offset+4], []byte{0x47, 0x1f, 0xff, 0x10 | byte(offset/188%16)})
	}
	return data
}

func mediaFixturePeerClosed(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

func writeMediaFixtureJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".media-fixture-*.json")
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
	return os.Rename(file.Name(), path)
}

func TestMediaResourceFixtureContract(t *testing.T) {
	if len(mediaFixtureManifest) != 906 {
		t.Fatalf("manifest has %d bytes, want 906", len(mediaFixtureManifest))
	}
	base, _ := mediaURL("https://127.0.0.1:443/clip/playlist.m3u8")
	selection, class := parsePlaylist([]byte(mediaFixtureManifest), base)
	if class != "" || selection.segment == nil || selection.segment.Path != "/clip/729.ts" || selection.segmentRange == nil || selection.segmentRange.start != 2976416 || selection.segmentRange.length != 745044 {
		t.Fatalf("fixture manifest contract: %s %+v", class, selection)
	}
	data := makeMediaFixturePrefix()
	if len(data) != 65536 || data[0] != 0x47 || data[188] != 0x47 {
		t.Fatal("invalid synthetic TS prefix")
	}
	requested := fmt.Sprintf("bytes=%d-%d", selection.segmentRange.start, selection.segmentRange.start+int64(len(data))-1)
	if requested != mediaFixtureRequestedRange {
		t.Fatalf("fixture range %s", requested)
	}
	if length, ok := contentRange(mediaFixtureContentRange, 2976416, 3041951); !ok || length != int64(len(data)) {
		t.Fatal("fixture Content-Range mismatch")
	}
}
