package wstransport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/httptest"
	utls "github.com/bogdanfinn/utls"
	"github.com/bogdanfinn/websocket"
)

var clients = []string{"stdlib", "chrome_152"}

type helloObservation struct {
	serverName string
	protocols  []string
}

type fixture struct {
	endpoint string
	roots    *x509.CertPool
	hellos   chan helloObservation
}

// A real local TLS socket exercises both crypto/tls and uTLS rather than
// replacing their dialers with mocks. The certificate covers localhost only.
func newFixture(t *testing.T, handler fhttp.HandlerFunc, protocols ...[]string) fixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wstransport local fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	hellos := make(chan helloObservation, 8)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &utls.Config{
		Certificates: []utls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"http/1.1"},
		GetConfigForClient: func(hello *utls.ClientHelloInfo) (*utls.Config, error) {
			hellos <- helloObservation{hello.ServerName, append([]string(nil), hello.SupportedProtos...)}
			return nil, nil
		},
	}
	if len(protocols) > 0 {
		server.TLS.NextProtos = protocols[0]
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return fixture{
		endpoint: "wss://localhost:" + strings.Split(server.Listener.Addr().String(), ":")[1] + "/events",
		roots:    roots, hellos: hellos,
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for local fixture")
		var zero T
		return zero
	}
}

func TestVerifiedTLSUpgrade(t *testing.T) {
	// Deliberately unusable environment proxies must not affect a direct dial.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	for _, client := range clients {
		t.Run(client, func(t *testing.T) {
			done := make(chan error, 1)
			requests := make(chan *fhttp.Request, 1)
			fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				requests <- r
				upgrader := websocket.Upgrader{CheckOrigin: func(r *fhttp.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, fhttp.Header{"Set-Cookie": []string{"fixture=ignored"}})
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				kind, data, err := conn.ReadMessage()
				if err == nil {
					err = conn.WriteMessage(kind, data)
				}
				done <- err
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, meta, err := Dial(ctx, fx.endpoint, Config{Client: client, RootCAs: fx.roots, Origin: "https://example.test", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if meta.Status != 101 || meta.HTTPVersion != "HTTP/1.1" || meta.ALPN != "http/1.1" || meta.ServerName != "localhost" || meta.TLSVersion == "" || meta.Cipher == "" || meta.DidResume {
				t.Fatalf("unexpected handshake metadata: %+v", meta)
			}
			hello := receive(t, fx.hellos)
			if hello.serverName != "localhost" || len(hello.protocols) != 1 || hello.protocols[0] != "http/1.1" {
				t.Fatalf("wire ClientHello: %+v", hello)
			}
			req := receive(t, requests)
			if req.Proto != "HTTP/1.1" || req.Method != "GET" || req.Header.Get("Origin") != "https://example.test" || req.Header.Get("Cookie") != "" || req.Header.Get("Sec-WebSocket-Extensions") != "" || req.Header.Get("Sec-WebSocket-Protocol") != "" {
				t.Fatalf("unexpected upgrade request: %s %s %v", req.Method, req.Proto, req.Header)
			}
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, []byte("echo")); err != nil {
				t.Fatal(err)
			}
			kind, data, err := conn.ReadMessage()
			if err != nil || kind != websocket.TextMessage || string(data) != "echo" {
				t.Fatalf("echo kind=%d data=%q err=%v", kind, data, err)
			}
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
			if len(fx.hellos) != 0 {
				t.Fatal("upgrade made a second TLS connection")
			}
		})
	}
}

func TestNoALPNHTTP1Fallback(t *testing.T) {
	for _, client := range clients {
		t.Run(client, func(t *testing.T) {
			fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err == nil {
					conn.Close()
				}
			}, []string{})
			conn, meta, err := Dial(context.Background(), fx.endpoint, Config{Client: client, RootCAs: fx.roots})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if meta.Status != 101 || meta.HTTPVersion != "HTTP/1.1" || meta.ALPN != "" {
				t.Fatalf("expected explicit empty ALPN with HTTP1 upgrade: %+v", meta)
			}
		})
	}
}

func TestCancelDuringTLSHandshake(t *testing.T) {
	for _, client := range clients {
		t.Run(client, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			started := make(chan error, 1)
			closed := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					started <- err
					return
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				buffer := make([]byte, 16384)
				_, err = conn.Read(buffer) // receive ClientHello; send no ServerHello
				started <- err
				_, err = io.Copy(io.Discard, conn)
				closed <- err
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				conn, _, err := Dial(ctx, "wss://"+listener.Addr().String(), Config{Client: client, Timeout: 2 * time.Second})
				if conn != nil {
					conn.Close()
				}
				result <- err
			}()
			if err := receive(t, started); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := receive(t, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("TLS handshake cancellation: %v", err)
			}
			if err := receive(t, closed); err != nil {
				t.Fatalf("TLS socket did not close after cancellation: %v", err)
			}
		})
	}
}

func TestCertificateVerification(t *testing.T) {
	for _, client := range clients {
		for _, scenario := range []string{"untrusted", "hostname"} {
			t.Run(client+"/"+scenario, func(t *testing.T) {
				var requests atomic.Int32
				fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) { requests.Add(1) })
				cfg := Config{Client: client, Timeout: time.Second}
				endpoint := fx.endpoint
				if scenario == "hostname" {
					cfg.RootCAs = fx.roots
					endpoint = strings.Replace(endpoint, "localhost", "127.0.0.1", 1)
				}
				conn, _, err := Dial(context.Background(), endpoint, cfg)
				if err == nil || conn != nil || requests.Load() != 0 {
					t.Fatalf("unverified certificate accepted: conn=%v requests=%d err=%v", conn, requests.Load(), err)
				}
				if !strings.Contains(err.Error(), "certificate") {
					t.Fatalf("expected certificate error: %v", err)
				}
			})
		}
	}
}

func TestHandshakeRejection(t *testing.T) {
	for _, client := range clients {
		for _, scenario := range []string{"redirect", "accept", "extension", "subprotocol"} {
			t.Run(client+"/"+scenario, func(t *testing.T) {
				var requests atomic.Int32
				done := make(chan error, 1)
				fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					requests.Add(1)
					if scenario == "redirect" {
						w.Header().Set("Location", "https://"+r.Host+"/redirected")
						w.WriteHeader(302)
						return
					}
					extra := ""
					if scenario == "extension" {
						extra = "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"
					} else if scenario == "subprotocol" {
						extra = "Sec-WebSocket-Protocol: unsolicited\r\n"
					}
					conn, rw, err := rawUpgrade(w, r, scenario == "accept", extra)
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					_, err = rw.ReadByte()
					if errors.Is(err, io.EOF) {
						err = nil
					}
					done <- err
				})
				conn, meta, err := Dial(context.Background(), fx.endpoint, Config{Client: client, RootCAs: fx.roots, Timeout: time.Second})
				if err == nil || conn != nil || requests.Load() != 1 {
					t.Fatalf("bad handshake accepted or redirected: requests=%d meta=%+v err=%v", requests.Load(), meta, err)
				}
				if scenario == "redirect" {
					if meta.Status != 302 || !errors.Is(err, websocket.ErrBadHandshake) {
						t.Fatalf("redirect metadata: %+v, %v", meta, err)
					}
				} else {
					if meta.Status != 101 {
						t.Fatalf("expected 101 recorded before validation: %+v", meta)
					}
					if err := receive(t, done); err != nil {
						t.Fatalf("rejected socket was not closed: %v", err)
					}
				}
			})
		}
	}
}

func TestUpgradeCancellationAndTimeout(t *testing.T) {
	for _, client := range clients {
		for _, scenario := range []string{"cancel", "timeout"} {
			t.Run(client+"/"+scenario, func(t *testing.T) {
				started := make(chan struct{}, 1)
				closed := make(chan struct{}, 1)
				fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					started <- struct{}{}
					<-r.Context().Done()
					closed <- struct{}{}
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				timeout := 200 * time.Millisecond
				if scenario == "cancel" {
					timeout = 2 * time.Second
				}
				result := make(chan error, 1)
				go func() {
					conn, _, err := Dial(ctx, fx.endpoint, Config{Client: client, RootCAs: fx.roots, Timeout: timeout})
					if conn != nil {
						conn.Close()
					}
					result <- err
				}()
				receive(t, started)
				if scenario == "cancel" {
					cancel()
				}
				err := receive(t, result)
				want := context.DeadlineExceeded
				if scenario == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
				receive(t, closed)
			})
		}
	}
}

func TestSessionLifetime(t *testing.T) {
	for _, client := range clients {
		t.Run(client, func(t *testing.T) {
			closed := make(chan struct{}, 1)
			fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				conn.ReadMessage()
				closed <- struct{}{}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, _, err := Dial(ctx, fx.endpoint, Config{Client: client, RootCAs: fx.roots, Timeout: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			result := make(chan error, 1)
			go func() { _, _, err := conn.ReadMessage(); result <- err }()
			// An established socket must survive the handshake timeout and the
			// dialer's internal context cancellation on a successful return.
			select {
			case err := <-result:
				t.Fatalf("session closed with handshake context: %v", err)
			case <-time.After(150 * time.Millisecond):
			}
			cancel()
			if err := receive(t, result); err == nil {
				t.Fatal("context cancellation left read open")
			}
			receive(t, closed)
		})
	}
}

func TestFramesControlPingFragmentationAndMasking(t *testing.T) {
	for _, client := range clients {
		t.Run(client, func(t *testing.T) {
			done := make(chan error, 1)
			fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				conn, rw, err := rawUpgrade(w, r, false, "")
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				// Two text fragments, with a control ping between them.
				_, err = rw.Write([]byte{0x01, 3, 'h', 'e', 'l', 0x89, 1, 'p', 0x80, 2, 'l', 'o'})
				if err == nil {
					err = rw.Flush()
				}
				if err == nil {
					err = expectMaskedFrame(rw, websocket.PongMessage, []byte("p"))
				}
				if err == nil {
					err = expectMaskedFrame(rw, websocket.TextMessage, []byte("client"))
				}
				done <- err
			})
			conn, _, err := Dial(context.Background(), fx.endpoint, Config{Client: client, RootCAs: fx.roots})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			kind, data, err := conn.ReadMessage()
			if err != nil || kind != websocket.TextMessage || string(data) != "hello" {
				t.Fatalf("fragment reconstruction: kind=%d data=%q err=%v", kind, data, err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte("client")); err != nil {
				t.Fatal(err)
			}
			if err := receive(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadLimitAndCloseFrame(t *testing.T) {
	for _, client := range clients {
		for _, scenario := range []string{"limit", "close"} {
			t.Run(client+"/"+scenario, func(t *testing.T) {
				done := make(chan error, 1)
				fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					conn, rw, err := rawUpgrade(w, r, false, "")
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					frames := []byte{0x01, 3, 'a', 'b', 'c', 0x80, 3, 'd', 'e', 'f'}
					closeCode := []byte{0x03, 0xf1} // 1009, message too big
					if scenario == "close" {
						frames = []byte{0x88, 2, 0x03, 0xe8}
						closeCode = []byte{0x03, 0xe8} // 1000, normal closure
					}
					_, err = rw.Write(frames)
					if err == nil {
						err = rw.Flush()
					}
					if err == nil {
						err = expectMaskedFrame(rw, websocket.CloseMessage, closeCode)
					}
					done <- err
				})
				conn, _, err := Dial(context.Background(), fx.endpoint, Config{Client: client, RootCAs: fx.roots, ReadLimit: 5})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, _, err = conn.ReadMessage()
				if scenario == "limit" && !errors.Is(err, websocket.ErrReadLimit) {
					t.Fatalf("aggregate fragmented message limit: %v", err)
				}
				if scenario == "close" && !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					t.Fatalf("expected normal close: %v", err)
				}
				if err := receive(t, done); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestValidationBeforeDial(t *testing.T) {
	cases := []struct {
		endpoint string
		cfg      Config
	}{
		{"ws://localhost/events", Config{}},
		{"wss://user:password@localhost/events", Config{}},
		{"wss://localhost/events#fragment", Config{}},
		{"wss://localhost/events", Config{Client: "chrome_156"}},
		{"wss://localhost/events", Config{Timeout: -time.Second}},
		{"wss://localhost/events", Config{Timeout: 3 * time.Minute}},
		{"wss://localhost/events", Config{ReadLimit: -1}},
		{"wss://localhost/events", Config{ReadLimit: 17 << 20}},
		{"wss://localhost/events", Config{Origin: "https://localhost/path"}},
		{"wss://localhost/events", Config{Origin: "https://localhost?token=value"}},
		{"wss://localhost/events", Config{Origin: "https://localhost?"}},
	}
	for _, tc := range cases {
		conn, meta, err := Dial(context.Background(), tc.endpoint, tc.cfg)
		if err == nil || conn != nil || meta != (Metadata{}) {
			t.Fatalf("invalid input was dialed: endpoint=%s cfg=%+v meta=%+v err=%v", tc.endpoint, tc.cfg, meta, err)
		}
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	const maxDuration = time.Duration(1<<63 - 1)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"60", time.Minute},
		{" 2 ", 2 * time.Second},
		{"0003", 3 * time.Second},
		{"0", 0},
		{"", 0},
		{"-1", 0},
		{"+1", 0},
		{"1.5", 0},
		{"not-a-date", 0},
		{now.Add(90 * time.Second).Format(fhttp.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(fhttp.TimeFormat), 0},
		{"9223372036", 9223372036 * time.Second},
		{"9223372037", maxDuration},
		{"184467440737095516160", maxDuration},
	} {
		got := parseRetryAfter(tc.header, now)
		if got != tc.want {
			t.Errorf("Retry-After %q: got %s want %s", tc.header, got, tc.want)
		}
		if decoded, err := time.ParseDuration(got.String()); err != nil || decoded != got {
			t.Errorf("normalized duration does not round-trip: %q %v", got.String(), err)
		}
	}
}

func TestRejectedUpgradeRetainsNormalizedRetryAfter(t *testing.T) {
	for _, client := range clients {
		for _, format := range []string{"seconds", "date", "invalid"} {
			t.Run(client+"/"+format, func(t *testing.T) {
				header := "60"
				if format == "date" {
					header = time.Now().Add(time.Minute).UTC().Format(fhttp.TimeFormat)
				} else if format == "invalid" {
					header = "untrusted-header-value"
				}
				fx := newFixture(t, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					w.Header().Set("Retry-After", header)
					w.WriteHeader(429)
				})
				conn, meta, err := Dial(context.Background(), fx.endpoint, Config{Client: client, RootCAs: fx.roots, Timeout: time.Second})
				if conn != nil || !errors.Is(err, websocket.ErrBadHandshake) || meta.Status != 429 {
					t.Fatalf("expected rejected upgrade: %+v %v", meta, err)
				}
				if format == "invalid" {
					if meta.RetryAfter != "" {
						t.Fatalf("untrusted Retry-After retained: %q", meta.RetryAfter)
					}
					return
				}
				delay, err := time.ParseDuration(meta.RetryAfter)
				if err != nil || delay <= 55*time.Second || delay > time.Minute {
					t.Fatalf("normalized Retry-After: %q %v", meta.RetryAfter, err)
				}
				if format == "seconds" && delay != time.Minute {
					t.Fatalf("seconds delay changed: %s", delay)
				}
			})
		}
	}
}

// rawUpgrade permits protocol-negative fixtures and inspection of the actual
// client mask bit. It is test-only, never an alternative WebSocket parser.
func rawUpgrade(w fhttp.ResponseWriter, r *fhttp.Request, wrongAccept bool, extra string) (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.(fhttp.Hijacker).Hijack()
	if err != nil {
		return nil, nil, err
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(digest[:])
	if wrongAccept {
		accept = "invalid"
	}
	_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n%s\r\n", accept, extra)
	if err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
	}
	return conn, rw, err
}

func expectMaskedFrame(reader io.Reader, opcode int, payload []byte) error {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	if header[0] != 0x80|byte(opcode) || header[1]&0x80 == 0 || int(header[1]&0x7f) != len(payload) {
		return fmt.Errorf("expected masked opcode=%d size=%d, header=%x", opcode, len(payload), header)
	}
	var mask [4]byte
	if _, err := io.ReadFull(reader, mask[:]); err != nil {
		return err
	}
	data := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, data); err != nil {
		return err
	}
	for i := range data {
		data[i] ^= mask[i%4]
	}
	if !bytes.Equal(data, payload) {
		return fmt.Errorf("unmasked payload %q, want %q", data, payload)
	}
	return nil
}
