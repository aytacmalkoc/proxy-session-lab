package mediaprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

type byteRange struct{ start, length int64 }
type responseData struct {
	request     Request
	body        []byte
	contentType string
	finalURL    *url.URL
	class       string
}
type fetcher func(context.Context, *url.URL, bool, *byteRange) responseData
type wireResponse struct {
	status int
	header func(string) string
	body   io.ReadCloser
	length int64
}

func newFetcher(cfg Config, roots *x509.CertPool) (fetcher, func(), error) {
	return newFetcherWithPrefixLimit(cfg, roots, prefixBytes, nil)
}

// origin, when set, also restricts every redirect before the next request.
func newFetcherWithPrefixLimit(cfg Config, roots *x509.CertPool, maxPrefix int64, origin *url.URL) (fetcher, func(), error) {
	var do func(context.Context, *url.URL, string) (wireResponse, error)
	var closeClient func()
	if cfg.Client == "stdlib" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy, transport.DisableCompression = nil, true
		if cfg.ProxyURL != "" {
			p, err := url.Parse(cfg.ProxyURL)
			if err != nil {
				return nil, nil, err
			}
			transport.Proxy = http.ProxyURL(p)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		closeClient = transport.CloseIdleConnections
		do = func(ctx context.Context, u *url.URL, rangeHeader string) (wireResponse, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			if err != nil {
				return wireResponse{}, err
			}
			req.Header.Set("Accept-Encoding", "identity")
			if rangeHeader != "" {
				req.Header.Set("Range", rangeHeader)
			}
			res, err := client.Do(req)
			if err != nil {
				return wireResponse{}, err
			}
			return wireResponse{res.StatusCode, res.Header.Get, res.Body, res.ContentLength}, nil
		}
	} else {
		options := []tlsclient.HttpClientOption{tlsclient.WithClientProfile(profiles.Chrome_152), tlsclient.WithNotFollowRedirects(), tlsclient.WithDisableHttp3(), tlsclient.WithDisableSessionTickets(), tlsclient.WithRandomTLSExtensionOrder(), tlsclient.WithTimeoutMilliseconds(int(cfg.Timeout.Milliseconds()) + 1), tlsclient.WithTransportOptions(&tlsclient.TransportOptions{RootCAs: roots, DisableCompression: true})}
		if cfg.ProxyURL != "" {
			options = append(options, tlsclient.WithProxyUrl(cfg.ProxyURL))
		}
		client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
		if err != nil {
			return nil, nil, err
		}
		closeClient = client.CloseIdleConnections
		do = func(ctx context.Context, u *url.URL, rangeHeader string) (wireResponse, error) {
			req, err := fhttp.NewRequestWithContext(ctx, "GET", u.String(), nil)
			if err != nil {
				return wireResponse{}, err
			}
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")
			if rangeHeader != "" {
				req.Header.Set("Range", rangeHeader)
			}
			res, err := client.Do(req)
			if err != nil {
				return wireResponse{}, err
			}
			return wireResponse{res.StatusCode, res.Header.Get, res.Body, res.ContentLength}, nil
		}
	}
	return func(ctx context.Context, target *url.URL, full bool, selected *byteRange) (result responseData) {
		started := time.Now()
		result.request.URL, result.finalURL = redactedURL(target), target
		defer func() { result.request.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond) }()
		if origin != nil && !sameMediaOrigin(origin, target) {
			result.class = "unsupported_origin"
			return result
		}
		limit, start, end := maxPrefix, int64(0), maxPrefix-1
		if full {
			limit = manifestBytes + 1
		}
		if selected != nil {
			start, limit = selected.start, min(selected.length, maxPrefix)
			end = start + limit - 1
		}
		rangeHeader := ""
		if !full {
			rangeHeader = fmt.Sprintf("bytes=%d-%d", start, end)
		}
		result.request.RequestedRange = rangeHeader
		for redirects := 0; ; redirects++ {
			res, err := do(ctx, target, rangeHeader)
			if err != nil {
				result.class = requestErrorClass(ctx)
				return result
			}
			result.request.Status = res.status
			result.request.URL, result.finalURL = redactedURL(target), target
			result.request.RetryAfter = ""
			if delay := retryAfter(res.header("Retry-After"), time.Now()); delay > 0 {
				result.request.RetryAfter = delay.String()
			}
			if res.status == 301 || res.status == 302 || res.status == 303 || res.status == 307 || res.status == 308 {
				location := res.header("Location")
				res.body.Close()
				reference, err := url.Parse(location)
				if err != nil || location == "" || redirects >= 3 {
					result.class = "redirect_policy"
					return result
				}
				next := target.ResolveReference(reference)
				if _, err := mediaURL(next.String()); err != nil {
					result.class = "redirect_policy"
					return result
				}
				if origin != nil && !sameMediaOrigin(origin, next) {
					result.class = "redirect_policy"
					return result
				}
				target = next
				continue
			}
			defer res.body.Close()
			if res.status != 200 && res.status != 206 {
				result.class = "http_status"
				return result
			}
			if encoding := strings.ToLower(strings.TrimSpace(res.header("Content-Encoding"))); encoding != "" && encoding != "identity" {
				result.class = "unsupported_encoding"
				return result
			}
			result.contentType = res.header("Content-Type")
			if full && res.status != 200 {
				result.class = "partial_manifest"
				return result
			}
			if selected != nil && res.status != 206 {
				result.class = "range_ignored"
				return result
			}
			expected := int64(-1)
			if res.status == 206 {
				var valid bool
				expected, valid = contentRange(res.header("Content-Range"), start, end)
				if !valid {
					result.class = "range_mismatch"
					return result
				}
				if selected != nil && expected != limit {
					result.class = "range_mismatch"
					return result
				}
				if res.length >= 0 && res.length != expected {
					result.class = "range_length_mismatch"
					return result
				}
				result.request.ContentRange = res.header("Content-Range")
			}
			readLimit := limit
			if expected >= 0 {
				// A LimitReader ending exactly at the requested range invents
				// EOF before HTTP/2 END_STREAM. The pinned fhttp transport then
				// sends RST_STREAM on Close. Read one bounded sentinel byte to
				// observe actual EOF (or reject an oversized 206 response).
				// The request context still bounds a peer that never ends it.
				readLimit = expected + 1
			}
			result.body, err = io.ReadAll(io.LimitReader(res.body, readLimit))
			result.request.BytesRead = len(result.body)
			if expected >= 0 && int64(len(result.body)) > expected {
				result.class = "range_length_mismatch"
				return result
			}
			if err != nil {
				result.class = "truncated_body"
				if ctx.Err() != nil {
					result.class = requestErrorClass(ctx)
				}
				return result
			}
			if len(result.body) == 0 {
				result.class = "empty_body"
				return result
			}
			if expected >= 0 && int64(len(result.body)) != expected {
				result.class = "truncated_body"
				return result
			}
			if full && len(result.body) > manifestBytes {
				result.class = "manifest_too_large"
				return result
			}
			return result
		}
	}, closeClient, nil
}

func sameMediaOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if u.Port() == "" {
			return "443"
		}
		return u.Port()
	}
	return a.Scheme == "https" && b.Scheme == "https" && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func requestErrorClass(ctx context.Context) string {
	if ctx.Err() == context.DeadlineExceeded {
		return "timeout"
	}
	if ctx.Err() != nil {
		return "cancelled"
	}
	return "network_error"
}

func contentRange(value string, requestedStart, requestedEnd int64) (int64, bool) {
	if len(value) > 128 || !strings.HasPrefix(value, "bytes ") {
		return 0, false
	}
	interval, totalText, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !ok {
		return 0, false
	}
	startText, endText, ok := strings.Cut(interval, "-")
	if !ok || startText == "" || endText == "" || strings.Trim(startText+endText, "0123456789") != "" {
		return 0, false
	}
	start, a := strconv.ParseInt(startText, 10, 64)
	end, b := strconv.ParseInt(endText, 10, 64)
	if a != nil || b != nil || start < 0 || end < start || start != requestedStart {
		return 0, false
	}
	wantEnd := requestedEnd
	if totalText != "*" {
		if totalText == "" || strings.Trim(totalText, "0123456789") != "" {
			return 0, false
		}
		total, err := strconv.ParseInt(totalText, 10, 64)
		if err != nil || total <= end {
			return 0, false
		}
		wantEnd = min(wantEnd, total-1)
	}
	if end != wantEnd {
		return 0, false
	}
	return end - start + 1, true
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		const maximum = time.Duration(1<<63 - 1)
		if err != nil || seconds > uint64(maximum/time.Second) {
			return maximum
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}
