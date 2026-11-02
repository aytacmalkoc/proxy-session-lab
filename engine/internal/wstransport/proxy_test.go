package wstransport

import (
	"context"
	"testing"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/testproxy"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"
)

func TestVerifiedWSSUsesSelectedProxyAndSingleTLS(t *testing.T) {
	for _, profile := range []string{"stdlib", "chrome_152"} {
		t.Run(profile, func(t *testing.T) {
			proxy := testproxy.New(t, "")
			upgrader := websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}
			f := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				c, e := upgrader.Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer c.Close()
				c.WriteMessage(websocket.TextMessage, []byte("fixture"))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, meta, err := Dial(ctx, f.endpoint, Config{Client: profile, Timeout: time.Second, ProxyURL: proxy.URL, RootCAs: f.roots})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, body, err := c.ReadMessage()
			if err != nil || string(body) != "fixture" || meta.Status != 101 || proxy.Connects.Load() != 1 {
				t.Fatalf("proxy WSS %v %+v %q", err, meta, body)
			}
			hello := receive(t, f.hellos)
			if len(hello.protocols) != 1 || hello.protocols[0] != "http/1.1" {
				t.Fatalf("unexpected ALPN %+v", hello)
			}
		})
	}
}
