// Package wstransport establishes direct WSS connections over one verified TLS
// handshake. Browser profiles here describe TLS only, not complete browser WSS
// behavior or header equivalence.
package wstransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/proxytransport"
	"net"
	"net/url"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
	"github.com/bogdanfinn/websocket"
)

const DefaultReadLimit int64 = 1 << 20

type Config struct {
	ProxyURL  string         `json:"-"`
	Client    string         // stdlib (default) or chrome_152
	Timeout   time.Duration  // handshake only; default 15s
	Origin    string         // optional HTTP(S) origin, without path/query
	RootCAs   *x509.CertPool // nil uses system roots; fixtures may supply a CA
	ReadLimit int64          // maximum message size; default 1 MiB, maximum 16 MiB
}

type Metadata struct {
	Client         string `json:"client"`
	LibraryVersion string `json:"library_version"`
	ServerName     string `json:"server_name"`
	TLSVersion     string `json:"tls_version"`
	Cipher         string `json:"cipher"`
	ALPN           string `json:"alpn"`
	DidResume      bool   `json:"did_resume"`
	HTTPVersion    string `json:"http_version"`
	Status         int    `json:"status"`
	RetryAfter     string `json:"retry_after,omitempty"` // normalized positive Go duration, never raw header text
}

// Dial returns a connection whose lifetime is bounded by ctx. Canceling ctx
// closes the socket, including a blocked read. Close releases context callbacks.
// Timeout applies only to TCP/TLS/HTTP upgrade, not the established session.
// Empty negotiated ALPN is accepted as HTTP/1.1 fallback; any other non-H1
// protocol is rejected. No proxies, cookies, redirects, or compression are used.
func Dial(ctx context.Context, endpoint string, cfg Config) (*websocket.Conn, Metadata, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, Metadata{}, errors.New("endpoint must be a wss URL without credentials or a fragment")
	}
	if cfg.Client == "" {
		cfg.Client = "stdlib"
	}
	if cfg.Client != "stdlib" && cfg.Client != "chrome_152" {
		return nil, Metadata{}, errors.New("client must be stdlib or chrome_152")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Timeout < 0 || cfg.Timeout > 2*time.Minute {
		return nil, Metadata{}, errors.New("handshake timeout must be positive and at most 2m")
	}
	if cfg.ReadLimit == 0 {
		cfg.ReadLimit = DefaultReadLimit
	}
	if cfg.ReadLimit < 0 || cfg.ReadLimit > 16<<20 {
		return nil, Metadata{}, errors.New("read limit must be positive and at most 16 MiB")
	}
	if cfg.Origin != "" {
		origin, err := url.Parse(cfg.Origin)
		if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Hostname() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
			return nil, Metadata{}, errors.New("origin must be an HTTP(S) origin without credentials, path, query, or fragment")
		}
	}
	meta := Metadata{Client: cfg.Client, LibraryVersion: runtime.Version(), ServerName: u.Hostname()}
	handshakeCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	var dialTLS func(context.Context, string, string) (net.Conn, error)
	if cfg.ProxyURL != "" {
		if cfg.Client == "chrome_152" {
			meta.LibraryVersion = moduleVersion("github.com/bogdanfinn/utls")
		}
		if _, err := proxytransport.Parse(cfg.ProxyURL); err != nil {
			return nil, meta, err
		}
		dialTLS = func(callCtx context.Context, network, addr string) (net.Conn, error) {
			var stages []proxytransport.Stage
			conn, _, err := proxytransport.Dial(callCtx, u, cfg.ProxyURL, cfg.Client, true, cfg.RootCAs, &stages)
			return conn, err
		}
	} else if cfg.Client == "stdlib" {
		d := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: cfg.Timeout},
			Config:    &tls.Config{RootCAs: cfg.RootCAs, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12},
		}
		dialTLS = d.DialContext
	} else {
		client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
			tlsclient.WithClientProfile(profiles.Chrome_152), tlsclient.WithForceHttp1(),
			tlsclient.WithDisableHttp3(), tlsclient.WithDisableSessionTickets(),
			tlsclient.WithRandomTLSExtensionOrder(), tlsclient.WithNotFollowRedirects(),
			tlsclient.WithTimeoutMilliseconds(int(cfg.Timeout.Milliseconds())+1),
			tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: cfg.RootCAs}),
		)
		if err != nil {
			return nil, meta, fmt.Errorf("create chrome_152 TLS dialer: %w", err)
		}
		defer client.CloseIdleConnections()
		meta.LibraryVersion = moduleVersion("github.com/bogdanfinn/tls-client")
		// This library path handshakes once and forces H1, without HTTP pooling.
		dialTLS = client.GetTLSDialer()
	}

	stopHandshakeWatch := func() bool { return true }
	defer func() { stopHandshakeWatch() }()
	dialer := websocket.Dialer{
		HandshakeTimeout: cfg.Timeout,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
		// NetDialTLSContext accepts an already handshaken connection. Setting
		// it, and leaving Proxy nil, prevents a second TLS wrapper.
		NetDialTLSContext: func(callCtx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialTLS(callCtx, network, addr)
			if err != nil {
				return nil, err
			}
			var stateOK bool
			switch c := conn.(type) {
			case *tls.Conn:
				s := c.ConnectionState()
				stateOK = s.HandshakeComplete && len(s.VerifiedChains) > 0
				meta.TLSVersion, meta.Cipher, meta.ALPN, meta.DidResume = tls.VersionName(s.Version), tls.CipherSuiteName(s.CipherSuite), s.NegotiatedProtocol, s.DidResume
			case *utls.UConn:
				s := c.ConnectionState()
				stateOK = s.HandshakeComplete && len(s.VerifiedChains) > 0
				meta.TLSVersion, meta.Cipher, meta.ALPN, meta.DidResume = tls.VersionName(s.Version), tls.CipherSuiteName(s.CipherSuite), s.NegotiatedProtocol, s.DidResume
			}
			if !stateOK || (meta.ALPN != "" && meta.ALPN != "http/1.1") {
				conn.Close()
				return nil, fmt.Errorf("expected verified HTTP/1.1 TLS connection, negotiated ALPN %q", meta.ALPN)
			}
			managed := &contextConn{Conn: conn}
			managed.watch(ctx)
			// The websocket dialer cancels callCtx as soon as it returns, even
			// on success. Watch our outer handshake context so that its
			// callback can be detached before returning the live socket.
			stopHandshakeWatch = managed.watch(handshakeCtx)
			return managed, nil
		},
	}
	headers := fhttp.Header{}
	if cfg.Origin != "" {
		headers.Set("Origin", cfg.Origin)
	}
	if cfg.Client == "chrome_152" {
		headers.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")
	}
	conn, resp, err := dialer.DialContext(handshakeCtx, u.String(), headers)
	stopHandshakeWatch()
	if resp != nil {
		meta.Status, meta.HTTPVersion = resp.StatusCode, resp.Proto
		if delay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); delay > 0 {
			meta.RetryAfter = delay.String()
		}
		if resp.Body != nil {
			resp.Body.Close()
		}
	}
	if err != nil {
		return nil, meta, fmt.Errorf("WSS handshake: %w", errors.Join(err, handshakeCtx.Err()))
	}
	if handshakeCtx.Err() != nil {
		conn.Close()
		return nil, meta, handshakeCtx.Err()
	}
	if resp.Proto != "HTTP/1.1" || resp.Header.Get("Sec-WebSocket-Extensions") != "" || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		conn.Close()
		return nil, meta, errors.New("unexpected HTTP version, WebSocket extension, or subprotocol in handshake")
	}
	conn.SetReadLimit(cfg.ReadLimit)
	conn.EnableWriteCompression(false)
	return conn, meta, nil
}

// contextConn avoids a goroutine per socket. It also stops every registered
// cancellation callback when either the caller or the context closes it.
type contextConn struct {
	net.Conn
	mu     sync.Mutex
	closed bool
	stops  []func() bool
}

func (c *contextConn) watch(ctx context.Context) func() bool {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		stop()
		return stop
	}
	c.stops = append(c.stops, stop)
	c.mu.Unlock()
	return stop
}

func (c *contextConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	stops := c.stops
	c.stops = nil
	c.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
	return c.Conn.Close()
}

func moduleVersion(path string) string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == path {
				return dep.Version
			}
		}
	}
	return "unknown"
}

// Saturating a valid but oversized delay prevents it from becoming an immediate
// retry. Invalid values and dates that have already passed carry no delay.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		const maxDuration = time.Duration(1<<63 - 1)
		if err != nil || seconds > uint64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := fhttp.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}
