// Package testproxy supplies a real loopback CONNECT tunnel for integration tests.
package testproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type Fixture struct {
	URL      string
	Connects atomic.Int64
}

func New(t *testing.T, auth string) *Fixture {
	t.Helper()
	f := &Fixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != "" && r.Header.Get("Proxy-Authorization") != auth {
			w.WriteHeader(407)
			return
		}
		if r.Method != "CONNECT" {
			w.WriteHeader(400)
			return
		}
		f.Connects.Add(1)
		upstream, e := net.DialTimeout("tcp", r.Host, time.Second)
		if e != nil {
			w.WriteHeader(502)
			return
		}
		conn, reader, e := w.(http.Hijacker).Hijack()
		if e != nil {
			upstream.Close()
			return
		}
		defer conn.Close()
		defer upstream.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{})
		go func() { io.Copy(upstream, reader); upstream.Close(); close(done) }()
		io.Copy(conn, upstream)
		conn.Close()
		<-done
	}))
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}
