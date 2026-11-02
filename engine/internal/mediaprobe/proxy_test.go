package mediaprobe

import (
	"context"
	"net/http"
	"testing"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/testproxy"
)

func TestMediaUsesSelectedProxyForBothProfiles(t *testing.T) {
	for _, profile := range []string{"stdlib", "chrome_152"} {
		t.Run(profile, func(t *testing.T) {
			proxy := testproxy.New(t, "")
			origin, roots := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" {
					t.Error("credential leak")
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 0-23/24")
				w.WriteHeader(206)
				w.Write(mp4())
			})
			cfg := single(origin.URL+"/video.mp4", profile)
			cfg.ProxyURL = proxy.URL
			r, err := probe(context.Background(), cfg, roots)
			if err != nil || r.Successes != 1 || proxy.Connects.Load() != 1 {
				t.Fatalf("proxy media %v %+v connects=%d", err, r, proxy.Connects.Load())
			}
		})
	}
}
