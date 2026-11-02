package proxycheck

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func tunnel(w http.ResponseWriter, r *http.Request, auth string) {
	if auth != "" && r.Header.Get("Proxy-Authorization") != auth {
		w.WriteHeader(407)
		return
	}
	if r.Method != "CONNECT" {
		w.WriteHeader(400)
		return
	}
	upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
	if err != nil {
		w.WriteHeader(502)
		return
	}
	client, br, err := w.(http.Hijacker).Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	defer client.Close()
	defer upstream.Close()
	fmt.Fprint(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{})
	go func() { io.Copy(upstream, br); upstream.Close(); close(done) }()
	io.Copy(client, upstream)
	client.Close()
	<-done
}
func origins(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials reached origin")
		}
		if r.URL.Path == "/deny" {
			w.WriteHeader(403)
		}
		if r.URL.Path == "/busy" {
			w.WriteHeader(503)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ip":"203.0.113.9"}`)
	}))
	s.EnableHTTP2 = true
	s.StartTLS()
	t.Cleanup(s.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	return s, roots
}
func TestProfilesThroughAuthenticatedConnectAndHTTP2(t *testing.T) {
	origin, roots := origins(t)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:p@ss"))
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tunnel(w, r, auth) }))
	defer p.Close()
	for _, profile := range []string{"stdlib", "chrome_152"} {
		t.Run(profile, func(t *testing.T) {
			cfg := Config{ID: "test", URL: origin.URL, Proxy: strings.Replace(p.URL, "http://", "http://alice:p%40ss@", 1), Profile: profile, TimeoutSeconds: 3, RootCAs: roots}
			r := Check(context.Background(), cfg)
			if !r.TargetOK || !r.TransportOK || r.HTTPVersion != "HTTP/2.0" || r.TLS.ALPN != "h2" || r.ReportedEgressIP != "203.0.113.9" {
				t.Fatalf("invalid result %+v", r)
			}
			if strings.Contains(r.Proxy, "alice") || strings.Contains(r.Proxy, "p@ss") {
				t.Fatal("credentials leaked")
			}
			if len(r.Stages) != 5 || r.Stages[1].Name != "connect" || r.Stages[2].Name != "tls" {
				t.Fatalf("stages %+v", r.Stages)
			}
			cfg.RootCAs = nil
			r = Check(context.Background(), cfg)
			if r.FailedStage != "tls" || r.ErrorCode != "certificate_rejected" || r.TransportOK {
				t.Fatalf("untrusted origin accepted %+v", r)
			}
			cfg.RootCAs = roots
			cfg.Proxy = p.URL
			r = Check(context.Background(), cfg)
			if r.FailedStage != "connect" || r.ErrorCode != "proxy_auth_required" || r.Stages[1].Status != 407 {
				t.Fatalf("407 lost %+v", r)
			}
		})
	}
}
func TestHTTPSProxyHasSeparateVerifiedTLSStage(t *testing.T) {
	origin, roots := origins(t)
	p := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tunnel(w, r, "") }))
	defer p.Close()
	roots.AddCert(p.Certificate())
	r := Check(context.Background(), Config{URL: origin.URL, Proxy: p.URL, Profile: "chrome_152", TimeoutSeconds: 3, RootCAs: roots})
	if !r.TargetOK || r.Stages[1].Name != "proxy_tls" || r.Stages[2].Name != "connect" {
		t.Fatalf("HTTPS proxy %+v", r)
	}
}
func TestTargetPolicyIsSeparateFromTransport(t *testing.T) {
	origin, roots := origins(t)
	for _, path := range []string{"/deny", "/busy"} {
		r := Check(context.Background(), Config{URL: origin.URL + path, Profile: "stdlib", TimeoutSeconds: 2, RootCAs: roots})
		if !r.TransportOK || r.TargetOK || r.FailedStage != "target_http" || r.StopRun != (path == "/busy") {
			t.Fatalf("policy %+v", r)
		}
	}
	r := Check(context.Background(), Config{URL: origin.URL, Profile: "stdlib", TimeoutSeconds: 2, RootCAs: roots, ExpectText: "not-present"})
	if !r.TransportOK || r.TargetOK || r.ErrorCode != "expected_content_missing" {
		t.Fatalf("marker %+v", r)
	}
}
func TestHTTPForwardAbsoluteFormAndBoundedBody(t *testing.T) {
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Error("proxy request must be absolute")
		}
		io.WriteString(w, strings.Repeat("x", 100000))
	}))
	defer p.Close()
	for _, profile := range []string{"stdlib", "chrome_152"} {
		r := Check(context.Background(), Config{URL: "http://no-client-dns.invalid/path", Proxy: p.URL, Profile: profile, TimeoutSeconds: 2})
		if !r.TransportOK || !r.BodyTruncated || r.BodyBytes != 65537 {
			t.Fatalf("bounded HTTP %+v", r)
		}
	}
}
func TestConnectCancellationAndConfiguration(t *testing.T) {
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer p.Close()
	start := time.Now()
	r := Check(context.Background(), Config{URL: "https://no-client-dns.invalid", Proxy: p.URL, Profile: "chrome_152", TimeoutSeconds: .05})
	if r.TransportOK || r.FailedStage != "connect" || time.Since(start) > time.Second {
		t.Fatalf("cancellation %+v", r)
	}
	r = Check(context.Background(), Config{URL: "http://host.invalid", Proxy: "http://user:secret@127.0.0.1:1", Profile: "stdlib", TimeoutSeconds: 1})
	if r.FailedStage != "tcp" || r.ErrorCode != "connection_refused" || strings.Contains(r.Proxy, "secret") {
		t.Fatalf("refused %+v", r)
	}
	for _, bad := range []string{"ftp://127.0.0.1:1", "http://127.0.0.1:1/path", "socks5://127.0.0.1", "http://127.0.0.1:99999"} {
		r = Check(context.Background(), Config{URL: "https://origin.invalid", Proxy: bad, Profile: "stdlib", TimeoutSeconds: 1})
		if r.FailedStage != "config" {
			t.Fatalf("invalid accepted %s", bad)
		}
	}
}
func TestOversizedHTTP1HeaderIsRejected(t *testing.T) {
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Huge", strings.Repeat("x", 40000))
		io.WriteString(w, "body")
	}))
	defer p.Close()
	r := Check(context.Background(), Config{URL: "http://origin.invalid", Proxy: p.URL, Profile: "stdlib", TimeoutSeconds: 2})
	if r.TransportOK || r.FailedStage != "http" {
		t.Fatalf("header limit %+v", r)
	}
}

func TestSOCKS5UsernamePasswordAndRemoteDomain(t *testing.T) {
	origin, roots := origins(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		br := bufio.NewReader(conn)
		header := make([]byte, 2)
		if _, e = io.ReadFull(br, header); e != nil {
			return
		}
		io.CopyN(io.Discard, br, int64(header[1]))
		conn.Write([]byte{5, 2})
		br.ReadByte()
		length, _ := br.ReadByte()
		user := make([]byte, length)
		io.ReadFull(br, user)
		length, _ = br.ReadByte()
		password := make([]byte, length)
		io.ReadFull(br, password)
		if string(user) != "alice" || string(password) != "secret" {
			t.Error("SOCKS auth invalid")
		}
		conn.Write([]byte{1, 0})
		request := make([]byte, 4)
		io.ReadFull(br, request)
		if request[3] != 3 {
			t.Error("SOCKS must forward domain name")
			return
		}
		n, _ := br.ReadByte()
		name := make([]byte, n)
		io.ReadFull(br, name)
		if string(name) != "example.com" {
			t.Errorf("domain %q", name)
		}
		portBytes := make([]byte, 2)
		io.ReadFull(br, portBytes)
		upstream, e := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(binary.BigEndian.Uint16(portBytes))))
		if e != nil {
			return
		}
		defer upstream.Close()
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		done := make(chan struct{})
		go func() { io.Copy(upstream, br); upstream.Close(); close(done) }()
		io.Copy(conn, upstream)
		conn.Close()
		<-done
	}()
	r := Check(context.Background(), Config{URL: strings.Replace(origin.URL, "127.0.0.1", "example.com", 1), Proxy: "socks5://alice:secret@" + listener.Addr().String(), Profile: "chrome_152", TimeoutSeconds: 3, RootCAs: roots})
	if !r.TargetOK || r.Stages[1].Name != "socks5" {
		t.Fatalf("SOCKS %+v", r)
	}
	wg.Wait()
}

func TestObserverFieldsAreBoundedAndUntrustedBodyIsNotLogged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"tls":{"ja4":"t13-observed","ja3_hash":"abc"},"http2":{"akamai_fingerprint":"settings|order"},"private_body":"secret-body"}`)
	}))
	defer server.Close()
	r := Check(context.Background(), Config{URL: server.URL, Profile: "stdlib", TimeoutSeconds: 1})
	if !r.TargetOK || r.Observed["ja4"] != "t13-observed" {
		t.Fatalf("observer %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "secret-body") {
		t.Fatal("raw body logged")
	}
}

func TestTLSProfilesActuallyChangeClientHelloOffers(t *testing.T) {
	var mu sync.Mutex
	var offers [][]uint16
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "healthy") }))
	origin.EnableHTTP2 = true
	origin.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		offers = append(offers, append([]uint16(nil), hello.CipherSuites...))
		mu.Unlock()
		return nil, nil
	}}
	origin.StartTLS()
	defer origin.Close()
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	for _, profile := range []string{"stdlib", "chrome_152"} {
		r := Check(context.Background(), Config{URL: origin.URL, Profile: profile, TimeoutSeconds: 2, RootCAs: roots})
		if !r.TargetOK {
			t.Fatalf("profile failed %+v", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(offers) != 2 || reflect.DeepEqual(offers[0], offers[1]) {
		t.Fatalf("profiles did not change actual ClientHello offers: %v", offers)
	}
}
