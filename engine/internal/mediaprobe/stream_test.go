package mediaprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const streamTestPartBytes = 188 * 400 // deliberately larger than the prefix probe

func streamTestData() []byte {
	data := make([]byte, 3*streamTestPartBytes)
	for i := range data {
		data[i] = byte(i/streamTestPartBytes + 1)
	}
	for offset := 0; offset < len(data); offset += 188 {
		data[offset] = 0x47
	}
	return data
}
func streamTestManifest() string {
	return fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-BYTERANGE:%d@0\n#EXTINF:2,\nmedia.ts?token=private-token\n#EXTINF:2,\n#EXT-X-BYTERANGE:%d\nmedia.ts?token=private-token\n#EXT-X-BYTERANGE:%d\n#EXTINF:2,\nmedia.ts?token=private-token\n#EXT-X-ENDLIST\n", streamTestPartBytes, streamTestPartBytes, streamTestPartBytes)
}

func TestStreamVODBothProfilesCompleteOrderedTransfer(t *testing.T) {
	for _, profile := range []string{"stdlib", "chrome_152"} {
		t.Run(profile, func(t *testing.T) {
			data := streamTestData()
			var calls, active, peak atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				current := active.Add(1)
				defer active.Add(-1)
				for before := peak.Load(); current > before && !peak.CompareAndSwap(before, current); before = peak.Load() {
				}
				if r.URL.Path == "/playlist.m3u8" {
					fmt.Fprint(w, streamTestManifest())
					return
				}
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= int64(len(data)) {
					t.Error("unexpected range")
					w.WriteHeader(416)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(206)
				w.Write(data[start : end+1])
			})
			var sink bytes.Buffer
			report, err := streamVOD(context.Background(), StreamConfig{URL: server.URL + "/playlist.m3u8?auth=private-token", Client: profile, Timeout: time.Second}, &sink, roots)
			if err != nil || report.StopReason != "complete" || !report.EndList || report.SegmentCount != 3 || report.CompletedSegments != 3 || report.MediaDurationSeconds != 6 || report.DeclaredBytes != int64(len(data)) || report.BytesDownloaded != int64(len(data)) || report.BytesWritten != int64(len(data)) || calls.Load() != 4 || peak.Load() != 1 || !bytes.Equal(sink.Bytes(), data) {
				t.Fatalf("ordered transfer: %v %+v calls=%d peak=%d sink=%d", err, report, calls.Load(), peak.Load(), sink.Len())
			}
			for i, part := range report.Segments {
				digest := sha256.Sum256(data[i*streamTestPartBytes : (i+1)*streamTestPartBytes])
				if part.Index != i+1 || part.StartSeconds != float64(i*2) || part.DurationSeconds != 2 || part.Bytes != streamTestPartBytes || part.BytesWritten != streamTestPartBytes || part.SHA256 != hex.EncodeToString(digest[:]) || part.Status != 206 || part.DownloadStartedMS > part.SinkWriteStartedMS {
					t.Fatalf("segment metadata: %+v", part)
				}
			}
			encoded, _ := json.Marshal(report)
			if strings.Contains(string(encoded), "private-token") || strings.Contains(string(encoded), "?auth=") {
				t.Fatal("credentials in stream report")
			}
		})
	}
}

func TestStreamVODStopsAtFirstTransferFailure(t *testing.T) {
	for _, mode := range []string{"truncated", "range", "ignored", "ts", "second-403"} {
		t.Run(mode, func(t *testing.T) {
			data := streamTestData()
			var calls atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/playlist.m3u8" {
					fmt.Fprint(w, streamTestManifest())
					return
				}
				var start, end int64
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				if mode == "second-403" && start > 0 {
					w.WriteHeader(403)
					fmt.Fprint(w, "private-body-token")
					return
				}
				if mode == "ignored" {
					w.Write(data)
					return
				}
				if mode == "range" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start+1, end, len(data)))
				} else {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				}
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(206)
				part := append([]byte(nil), data[start:end+1]...)
				if mode == "truncated" {
					part = part[:len(part)-188]
				}
				if mode == "ts" {
					part[188] = 0
				}
				w.Write(part)
			})
			var sink bytes.Buffer
			report, err := streamVOD(context.Background(), StreamConfig{URL: server.URL + "/playlist.m3u8", Timeout: time.Second}, &sink, roots)
			want := map[string]string{"truncated": "truncated_body", "range": "range_mismatch", "ignored": "range_ignored", "ts": "invalid_ts", "second-403": "restricted"}[mode]
			wantCalls, wantCompleted := int32(2), 0
			if mode == "second-403" {
				wantCalls, wantCompleted = 3, 1
			}
			if err == nil || report.StopReason != want || calls.Load() != wantCalls || report.CompletedSegments != wantCompleted || len(report.Segments) != wantCompleted+1 || sink.Len() != wantCompleted*streamTestPartBytes {
				t.Fatalf("failure continuation: %v %+v calls=%d sink=%d", err, report, calls.Load(), sink.Len())
			}
		})
	}
}

func TestStreamVODPreflightsBeforeAnySegment(t *testing.T) {
	for _, tag := range []string{"#EXT-X-MAP:URI=\"init.mp4\"", "#EXT-X-KEY:METHOD=AES-128,URI=\"key\"", "#EXT-X-PART:DURATION=1,URI=\"part.ts\"", "#EXT-X-DISCONTINUITY", "#EXT-X-STREAM-INF:BANDWIDTH=100", "live", "cross-origin", "duration-limit", "segment-limit", "total-limit", "count-limit"} {
		t.Run(tag, func(t *testing.T) {
			manifest := strings.Replace(streamTestManifest(), "#EXT-X-ENDLIST", tag+"\n#EXT-X-ENDLIST", 1)
			switch tag {
			case "live":
				manifest = strings.Replace(streamTestManifest(), "#EXT-X-ENDLIST", "", 1)
			case "cross-origin":
				manifest = strings.Replace(streamTestManifest(), "media.ts?token=private-token", "https://example.invalid/media.ts", 1)
			case "duration-limit":
				manifest = strings.Replace(streamTestManifest(), "#EXTINF:2,", "#EXTINF:61,", 1)
			case "segment-limit":
				manifest = strings.Replace(streamTestManifest(), fmt.Sprintf("%d@0", streamTestPartBytes), fmt.Sprintf("%d@0", maxStreamSegment+1), 1)
			case "total-limit":
				manifest = "#EXTM3U\n" + strings.Repeat(fmt.Sprintf("#EXTINF:1,\n#EXT-X-BYTERANGE:%d@0\nmedia.ts\n", maxStreamSegment/188*188), 9) + "#EXT-X-ENDLIST\n"
			case "count-limit":
				manifest = "#EXTM3U\n" + strings.Repeat("#EXTINF:0.5,\n#EXT-X-BYTERANGE:188@0\nmedia.ts\n", 65) + "#EXT-X-ENDLIST\n"
			}
			var calls atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, manifest) })
			var sink bytes.Buffer
			report, err := streamVOD(context.Background(), StreamConfig{URL: server.URL + "/playlist.m3u8", Timeout: time.Second}, &sink, roots)
			if err == nil || calls.Load() != 1 || len(report.Segments) != 0 || sink.Len() != 0 {
				t.Fatalf("unsupported manifest downloaded media: %v %+v calls=%d", err, report, calls.Load())
			}
		})
	}
}

type streamWriterFunc func([]byte) (int, error)

func (f streamWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestStreamVODSinkFailureAndCancellationStopNextRequest(t *testing.T) {
	for _, mode := range []string{"failure", "short", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			data := streamTestData()
			var calls atomic.Int32
			server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/playlist.m3u8" {
					fmt.Fprint(w, streamTestManifest())
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", streamTestPartBytes-1, len(data)))
				w.WriteHeader(206)
				w.Write(data[:streamTestPartBytes])
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := streamWriterFunc(func([]byte) (int, error) {
				if mode == "cancel" {
					cancel()
					return 0, context.Canceled
				}
				if mode == "short" {
					return 188, nil
				}
				return 0, errors.New("private-sink-token")
			})
			report, err := streamVOD(ctx, StreamConfig{URL: server.URL + "/playlist.m3u8", Timeout: time.Second}, sink, roots)
			if err == nil || calls.Load() != 2 || report.CompletedSegments != 0 || report.BytesDownloaded != streamTestPartBytes || strings.Contains(err.Error(), "private-sink-token") {
				t.Fatalf("sink failure: %v %+v calls=%d", err, report, calls.Load())
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation identity: %v", err)
			}
			if mode == "short" && report.BytesWritten != 188 {
				t.Fatalf("partial sink count: %+v", report)
			}
		})
	}
}

func TestStreamVODForbidsCrossOriginRedirect(t *testing.T) {
	var escaped atomic.Int32
	other, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { escaped.Add(1) })
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/playlist.m3u8" {
			fmt.Fprint(w, streamTestManifest())
			return
		}
		w.Header().Set("Location", other.URL+"/private-token")
		w.WriteHeader(302)
	})
	report, err := streamVOD(context.Background(), StreamConfig{URL: server.URL + "/playlist.m3u8", Timeout: time.Second}, io.Discard, roots)
	if err == nil || report.StopReason != "redirect_policy" || escaped.Load() != 0 || report.BytesWritten != 0 {
		t.Fatalf("cross-origin redirect: %v %+v escaped=%d", err, report, escaped.Load())
	}
}

func TestStreamVODParentDeadlineAndValidation(t *testing.T) {
	server, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	report, err := streamVOD(ctx, StreamConfig{URL: server.URL + "/playlist.m3u8", Timeout: time.Second}, io.Discard, roots)
	if !errors.Is(err, context.DeadlineExceeded) || report.StopReason != "cancelled" || report.BytesWritten != 0 {
		t.Fatalf("parent deadline: %v %+v", err, report)
	}
	if _, err := StreamVOD(nil, StreamConfig{URL: server.URL}, io.Discard); err == nil {
		t.Fatal("nil ctx accepted")
	}
	if _, err := StreamVOD(context.Background(), StreamConfig{URL: server.URL}, nil); err == nil {
		t.Fatal("nil sink accepted")
	}
	if ValidateStreamConfig(StreamConfig{URL: server.URL, Timeout: time.Minute}) == nil {
		t.Fatal("unbounded request timeout")
	}
}
