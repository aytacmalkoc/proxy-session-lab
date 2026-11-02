// Package proxycheck separates proxy transport evidence from target HTTP policy.
package proxycheck

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/proxytransport"
	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/fhttp/http2"
	"github.com/bogdanfinn/tls-client/profiles"
)

type Config struct {
	ID             string         `json:"id"`
	URL            string         `json:"url"`
	Proxy          string         `json:"proxy"`
	Profile        string         `json:"profile"`
	TimeoutSeconds float64        `json:"timeout_seconds"`
	ExpectText     string         `json:"expect_text,omitempty"`
	RootCAs        *x509.CertPool `json:"-"`
}
type Report struct {
	Observed             map[string]string      `json:"observed,omitempty"`
	Test                 string                 `json:"test"`
	Details              any                    `json:"details,omitempty"`
	ID                   string                 `json:"id"`
	Proxy                string                 `json:"proxy"`
	URL                  string                 `json:"url"`
	Profile              string                 `json:"profile"`
	TransportOK          bool                   `json:"transport_ok"`
	HTTPResponseReceived bool                   `json:"http_response_received"`
	BodyReadOK           bool                   `json:"body_read_ok"`
	BodyComplete         bool                   `json:"body_complete"`
	TargetOK             bool                   `json:"target_ok"`
	HTTPStatus           int                    `json:"http_status,omitempty"`
	HTTPVersion          string                 `json:"http_version,omitempty"`
	FailedStage          string                 `json:"failed_stage,omitempty"`
	ErrorCode            string                 `json:"error_code,omitempty"`
	StopRun              bool                   `json:"stop_run,omitempty"`
	TLS                  proxytransport.TLSInfo `json:"tls"`
	Stages               []proxytransport.Stage `json:"stages"`
	BodyBytes            int                    `json:"body_bytes"`
	BodySHA256           string                 `json:"body_sha256,omitempty"`
	BodyTruncated        bool                   `json:"body_truncated"`
	ReportedEgressIP     string                 `json:"reported_egress_ip,omitempty"`
	RetryAfter           string                 `json:"retry_after,omitempty"`
	ElapsedMS            float64                `json:"elapsed_ms"`
}

func safeURL(raw string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return "invalid-url"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
func Check(parent context.Context, cfg Config) (report Report) {
	start := time.Now()
	report = Report{ID: cfg.ID, Proxy: proxytransport.Redact(cfg.Proxy), URL: safeURL(cfg.URL), Profile: cfg.Profile, Stages: []proxytransport.Stage{}}
	defer func() { report.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond) }()
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || strings.ContainsAny(cfg.URL, "\r\n\t ") || cfg.TimeoutSeconds <= 0 || cfg.TimeoutSeconds > 60 || math.IsNaN(cfg.TimeoutSeconds) || math.IsInf(cfg.TimeoutSeconds, 0) {
		report.FailedStage = "config"
		report.ErrorCode = "invalid_configuration"
		return
	}
	if _, err = proxytransport.Parse(cfg.Proxy); err != nil || (cfg.Profile != "stdlib" && cfg.Profile != "chrome_152") {
		report.FailedStage = "config"
		report.ErrorCode = "invalid_proxy_or_profile"
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.TimeoutSeconds*float64(time.Second)))
	defer cancel()
	conn, tlsInfo, err := proxytransport.Dial(ctx, u, cfg.Proxy, cfg.Profile, false, cfg.RootCAs, &report.Stages)
	report.TLS = tlsInfo
	if err != nil {
		var e *proxytransport.Error
		if errors.As(err, &e) {
			report.FailedStage = e.Stage
			report.ErrorCode = e.Code
		} else {
			report.FailedStage = "connect"
			report.ErrorCode = proxytransport.Code(err)
		}
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	req, err := fhttp.NewRequestWithContext(ctx, "GET", cfg.URL, nil)
	if err != nil {
		report.FailedStage = "config"
		report.ErrorCode = "invalid_url"
		return
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	if cfg.Profile == "chrome_152" {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Sec-CH-UA", `"Chromium";v="152", "Google Chrome";v="152", "Not_A Brand";v="99"`)
		req.Header.Set("Sec-CH-UA-Mobile", "?0")
		req.Header.Set("Sec-CH-UA-Platform", `"macOS"`)
	} else {
		req.Header.Set("User-Agent", "proxy-session-lab-network/0.3")
	}
	req.Header[fhttp.HeaderOrderKey] = []string{"host", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "user-agent", "accept", "accept-language", "accept-encoding"}
	requestStart := time.Now()
	var res *fhttp.Response
	if tlsInfo.ALPN == "h2" {
		tr := &http2.Transport{Settings: map[http2.SettingID]uint32{http2.SettingMaxHeaderListSize: 65536}, SettingsOrder: []http2.SettingID{http2.SettingMaxHeaderListSize}}
		if cfg.Profile == "chrome_152" {
			p := profiles.Chrome_152
			tr.Settings = p.GetSettings()
			tr.SettingsOrder = p.GetSettingsOrder()
			tr.PseudoHeaderOrder = p.GetPseudoHeaderOrder()
			tr.ConnectionFlow = p.GetConnectionFlow()
			tr.HeaderPriority = p.GetHeaderPriority()
			tr.Priorities = p.GetPriorities()
		}
		var cc *http2.ClientConn
		cc, err = tr.NewClientConn(conn)
		if err == nil {
			defer cc.Close()
			res, err = cc.RoundTrip(req)
		}
	} else {
		p, _ := proxytransport.Parse(cfg.Proxy)
		if u.Scheme == "http" && p != nil && (p.Scheme == "http" || p.Scheme == "https") {
			if p.User != nil {
				pw, _ := p.User.Password()
				req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(p.User.Username()+":"+pw)))
			}
			err = req.WriteProxy(conn)
		} else {
			err = req.Write(conn)
		}
		if err == nil {
			reader, finish := proxytransport.HeaderReader(conn)
			res, err = fhttp.ReadResponse(reader, req)
			finish()
		}
	}
	err = proxytransport.ContextError(ctx, err)
	step := proxytransport.Stage{Name: "http", ElapsedMS: float64(time.Since(requestStart)) / float64(time.Millisecond), OK: err == nil}
	if err != nil {
		step.Code = proxytransport.Code(err)
		report.Stages = append(report.Stages, step)
		report.FailedStage = "http"
		report.ErrorCode = step.Code
		return
	}
	report.HTTPStatus = res.StatusCode
	report.HTTPResponseReceived = true
	report.HTTPVersion = res.Proto
	step.Status = res.StatusCode
	report.Stages = append(report.Stages, step)
	defer res.Body.Close()
	report.StopRun = res.StatusCode == 429 || res.StatusCode == 503
	if value := res.Header.Get("Retry-After"); len(value) <= 128 {
		if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
			report.RetryAfter = strconv.FormatInt(seconds, 10)
		} else if at, err := http.ParseTime(value); err == nil {
			seconds := int64(time.Until(at).Seconds())
			if seconds > 0 {
				report.RetryAfter = strconv.FormatInt(seconds, 10)
			}
		}
	}

	bodyStart := time.Now()
	body, err := io.ReadAll(io.LimitReader(res.Body, (64<<10)+1))
	err = proxytransport.ContextError(ctx, err)
	report.BodyBytes = len(body)
	report.BodyTruncated = len(body) > 64<<10
	report.BodyReadOK = err == nil
	report.BodyComplete = err == nil && !report.BodyTruncated
	step = proxytransport.Stage{Name: "body", ElapsedMS: float64(time.Since(bodyStart)) / float64(time.Millisecond), OK: err == nil}
	if err != nil {
		step.Code = proxytransport.Code(err)
		report.FailedStage = "body"
		report.ErrorCode = step.Code
	}
	report.Stages = append(report.Stages, step)
	if err != nil {
		return
	}
	report.TransportOK = true
	digest := sha256.Sum256(body)
	report.BodySHA256 = hex.EncodeToString(digest[:])
	report.TargetOK = res.StatusCode >= 200 && res.StatusCode < 400
	if !report.TargetOK {
		report.FailedStage = "target_http"
		report.ErrorCode = "target_http_error"
	}
	if res.Header.Get("Cf-Mitigated") == "challenge" {
		report.TargetOK = false
		report.FailedStage = "target_http"
		report.ErrorCode = "target_challenge"
	}
	if cfg.ExpectText != "" && !strings.Contains(string(body), cfg.ExpectText) {
		report.TargetOK = false
		report.FailedStage = "content"
		report.ErrorCode = "expected_content_missing"
	}
	if !report.BodyTruncated && strings.Contains(res.Header.Get("Content-Type"), "json") {
		var observer struct {
			TLS struct {
				JA4 string `json:"ja4"`
				JA3 string `json:"ja3_hash"`
			} `json:"tls"`
			HTTP2 struct {
				Fingerprint string `json:"akamai_fingerprint"`
			} `json:"http2"`
		}
		if json.Unmarshal(body, &observer) == nil {
			report.Observed = map[string]string{}
			for key, value := range map[string]string{"ja4": observer.TLS.JA4, "ja3_hash": observer.TLS.JA3, "h2_fingerprint": observer.HTTP2.Fingerprint} {
				printable := value != "" && len(value) <= 512
				for _, r := range value {
					if r < 32 || r > 126 {
						printable = false
					}
				}
				if printable {
					report.Observed[key] = value
				}
			}
			if len(report.Observed) == 0 {
				report.Observed = nil
			}
		}
	}

	if !report.BodyTruncated && len(body) <= 4096 && strings.Contains(res.Header.Get("Content-Type"), "json") && res.Header.Get("Content-Encoding") == "" {
		var data struct {
			IP string `json:"ip"`
		}
		if json.Unmarshal(body, &data) == nil && net.ParseIP(data.IP) != nil {
			report.ReportedEgressIP = data.IP
		}
	}
	return
}
