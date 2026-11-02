package playbackaudit

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/testproxy"
)

func TestCompleteLocalDecodeThroughProxyBothProfiles(t *testing.T) {
	decoder, fixture := os.Getenv("KICKPOP_FFMPEG"), os.Getenv("KICKPOP_DECODER_FIXTURE")
	if decoder == "" || fixture == "" {
		t.Skip("opt-in local synthetic decoder fixture")
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"stdlib", "chrome_152"} {
		t.Run(profile, func(t *testing.T) {
			proxy := testproxy.New(t, "")
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/playlist.m3u8" {
					fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\n#EXT-X-BYTERANGE:%d@0\nmedia.ts\n#EXT-X-ENDLIST\n", len(data))
					return
				}
				if r.Header.Get("Range") != fmt.Sprintf("bytes=0-%d", len(data)-1) {
					t.Errorf("wrong range %q", r.Header.Get("Range"))
					w.WriteHeader(416)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				w.WriteHeader(206)
				w.Write(data)
			}))
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			r, err := Run(context.Background(), Config{URL: server.URL + "/playlist.m3u8", Client: profile, ProxyURL: proxy.URL, RootCAs: roots, Decoder: decoder, Timeout: 10 * time.Second, RequestTimeout: 3 * time.Second})
			if err != nil || !r.Completed || r.Stream.CompletedSegments != 1 || r.Decoder.Stats.VideoFrames != 50 || proxy.Connects.Load() < 1 {
				t.Fatalf("full proxy decode err=%v complete=%v stop=%s video=%d connects=%d", err, r.Completed, r.StopReason, r.Decoder.Stats.VideoFrames, proxy.Connects.Load())
			}
		})
	}
}
