// Package proxytransport owns one bounded proxy tunnel and verified origin TLS.
// Credentials are sent only to the proxy and never returned in diagnostic errors.
package proxytransport

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
	"golang.org/x/net/proxy"
)

type Stage struct {
	Name      string  `json:"name"`
	OK        bool    `json:"ok"`
	ElapsedMS float64 `json:"elapsed_ms"`
	Code      string  `json:"code,omitempty"`
	Status    int     `json:"status,omitempty"`
}
type TLSInfo struct {
	Version    string `json:"version,omitempty"`
	Cipher     string `json:"cipher,omitempty"`
	ALPN       string `json:"alpn,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}
type Error struct {
	Stage  string
	Code   string
	Status int
	Cause  error
}

func (e *Error) Error() string { return e.Stage + ": " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }

func Parse(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(raw, "\r\n\t ") {
		return nil, errors.New("proxy must be an http, https or socks5 URL with an explicit port")
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p < 1 || p > 65535 {
		return nil, errors.New("proxy requires a valid explicit port")
	}
	if u.User != nil {
		if strings.ContainsAny(u.User.Username(), "\r\n") {
			return nil, errors.New("invalid proxy credentials")
		}
		pw, _ := u.User.Password()
		if strings.ContainsAny(pw, "\r\n") {
			return nil, errors.New("invalid proxy credentials")
		}
	}
	return u, nil
}
func Redact(raw string) string {
	u, err := Parse(raw)
	if err != nil {
		return "invalid-proxy"
	}
	if u == nil {
		return "direct"
	}
	u.User = nil
	u.Path = ""
	return u.String()
}
func Code(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	var ca x509.UnknownAuthorityError
	var hn x509.HostnameError
	var ci x509.CertificateInvalidError
	if errors.As(err, &ca) || errors.As(err, &hn) || errors.As(err, &ci) {
		return "certificate_rejected"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	return "connection_error"
}

// ContextError preserves cancellation/deadline evidence when closing the socket
// races with an I/O error such as "use of closed network connection".
func ContextError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func add(stages *[]Stage, name string, start time.Time, err error, status int) error {
	s := Stage{Name: name, OK: err == nil, ElapsedMS: float64(time.Since(start)) / float64(time.Millisecond), Status: status}
	if err != nil {
		s.Code = Code(err)
		var e *Error
		if errors.As(err, &e) {
			s.Code = e.Code
		}
		*stages = append(*stages, s)
		return &Error{Stage: name, Code: s.Code, Status: status, Cause: err}
	}
	*stages = append(*stages, s)
	return nil
}

type countedReader struct {
	r         io.Reader
	remaining int
}

func (r *countedReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("proxy response header exceeds 32 KiB")
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, e := r.r.Read(p)
	r.remaining -= n
	return n, e
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// Dial establishes TCP, optional proxy TLS/CONNECT/SOCKS5 and optional origin TLS.
// Caller closes the returned connection and supplies a deadline-bound context.
func Dial(ctx context.Context, target *url.URL, raw, profile string, forceH1 bool, roots *x509.CertPool, stages *[]Stage) (conn net.Conn, info TLSInfo, err error) {
	p, err := Parse(raw)
	if err != nil {
		return nil, info, err
	}
	if profile != "stdlib" && profile != "chrome_152" {
		return nil, info, errors.New("unknown TLS profile")
	}
	originAddr := target.Host
	if target.Port() == "" {
		port := "80"
		if target.Scheme == "https" || target.Scheme == "wss" {
			port = "443"
		}
		originAddr = net.JoinHostPort(target.Hostname(), port)
	}
	addr := originAddr
	if p != nil {
		addr = p.Host
	}
	start := time.Now()
	conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err = add(stages, "tcp", start, ContextError(ctx, err), 0); err != nil {
		return nil, info, err
	}
	rawConn := conn
	success := false
	defer func() {
		if !success {
			rawConn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if p != nil && p.Scheme == "https" {
		start = time.Now()
		outer := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: p.Hostname(), NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
		conn = outer
		err = outer.HandshakeContext(ctx)
		if err = add(stages, "proxy_tls", start, ContextError(ctx, err), 0); err != nil {
			return nil, info, err
		}
	}
	secure := target.Scheme == "https" || target.Scheme == "wss"
	if p != nil && p.Scheme == "socks5" {
		start = time.Now()
		var auth *proxy.Auth
		if p.User != nil {
			password, _ := p.User.Password()
			auth = &proxy.Auth{User: p.User.Username(), Password: password}
		}
		// The existing proxy TCP socket is reused; SOCKS5 domains resolve remotely.
		d, e := proxy.SOCKS5("tcp", p.Host, auth, singleConn{conn})
		if e == nil {
			conn, e = d.(proxy.ContextDialer).DialContext(ctx, "tcp", originAddr)
		}
		if err = add(stages, "socks5", start, ContextError(ctx, e), 0); err != nil {
			return nil, info, err
		}
	} else if p != nil && secure {
		start = time.Now()
		req := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: originAddr}, Host: originAddr, Header: make(http.Header)}
		if p.User != nil {
			password, _ := p.User.Password()
			req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.User.Username()+":"+password)))
		}
		var status int
		e := req.Write(conn)
		if e == nil {
			limited := &countedReader{r: conn, remaining: 32 << 10}
			br := bufio.NewReader(limited)
			var res *http.Response
			res, e = http.ReadResponse(br, req)
			if e == nil {
				status = res.StatusCode
				if status != 200 {
					code := "connect_rejected"
					if status == 407 {
						code = "proxy_auth_required"
					}
					if status == 429 {
						code = "proxy_rate_limited"
					}
					if status == 503 {
						code = "proxy_unavailable"
					}
					e = &Error{Code: code, Status: status}
				} else {
					limited.remaining = int(^uint(0) >> 1)
					conn = &bufferedConn{Conn: conn, reader: br}
				}
			}
		}
		if err = add(stages, "connect", start, ContextError(ctx, e), status); err != nil {
			return nil, info, err
		}
	}
	if secure {
		start = time.Now()
		info.ServerName = target.Hostname()
		if profile == "stdlib" {
			next := []string{"h2", "http/1.1"}
			if forceH1 {
				next = []string{"http/1.1"}
			}
			c := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: target.Hostname(), NextProtos: next, MinVersion: tls.VersionTLS12})
			conn = c
			err = c.HandshakeContext(ctx)
			if err == nil {
				s := c.ConnectionState()
				info.Version = tls.VersionName(s.Version)
				info.Cipher = tls.CipherSuiteName(s.CipherSuite)
				info.ALPN = s.NegotiatedProtocol
			}
		} else {
			spec, e := profiles.Chrome_152.GetClientHelloSpec()
			if e != nil {
				return nil, info, e
			}
			if forceH1 {
				for _, extension := range spec.Extensions {
					if alpn, ok := extension.(*utls.ALPNExtension); ok {
						alpn.AlpnProtocols = []string{"http/1.1"}
					}
				}
			}
			c := utls.UClient(conn, &utls.Config{RootCAs: roots, ServerName: target.Hostname()}, utls.HelloCustom, true, forceH1, true)
			conn = c
			err = c.ApplyPreset(&spec)
			if err == nil {
				err = c.HandshakeContext(ctx)
			}
			if err == nil {
				s := c.ConnectionState()
				info.Version = tls.VersionName(s.Version)
				info.Cipher = tls.CipherSuiteName(s.CipherSuite)
				info.ALPN = s.NegotiatedProtocol
			}
		}
		if err = add(stages, "tls", start, ContextError(ctx, err), 0); err != nil {
			return nil, info, err
		}
		if forceH1 && info.ALPN != "" && info.ALPN != "http/1.1" {
			return nil, info, fmt.Errorf("unexpected negotiated protocol")
		}
	}
	conn.SetDeadline(time.Time{})
	success = true
	return conn, info, nil
}

type singleConn struct{ net.Conn }

func (s singleConn) Dial(string, string) (net.Conn, error) { return s.Conn, nil }
func (s singleConn) DialContext(context.Context, string, string) (net.Conn, error) {
	return s.Conn, nil
}

// HeaderReader limits the response header phase, then keeps buffered body bytes.
func HeaderReader(conn net.Conn) (*bufio.Reader, func()) {
	limited := &countedReader{r: conn, remaining: 32 << 10}
	return bufio.NewReader(limited), func() { limited.remaining = int(^uint(0) >> 1) }
}
