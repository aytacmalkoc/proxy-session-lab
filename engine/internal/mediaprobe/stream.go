package mediaprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/proxytransport"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxStreamSegment = 8 << 20
const maxStreamBytes = 64 << 20

type StreamConfig struct {
	ProxyURL    string `json:"-"`
	URL, Client string
	Timeout     time.Duration  // per HTTP request; default 10s, maximum 30s
	RootCAs     *x509.CertPool `json:"-"` // optional explicit trust roots for controlled fixtures
}
type StreamSegment struct {
	Index              int     `json:"index"`
	URL                string  `json:"url"`
	StartSeconds       float64 `json:"start_seconds"`
	DurationSeconds    float64 `json:"duration_seconds"`
	Offset             int64   `json:"offset"`
	Length             int64   `json:"length"`
	RequestedRange     string  `json:"requested_range"`
	ContentRange       string  `json:"content_range,omitempty"`
	Status             int     `json:"status"`
	Bytes              int     `json:"bytes"`
	SHA256             string  `json:"sha256,omitempty"`
	DownloadStartedMS  float64 `json:"download_started_ms"`
	DownloadMS         float64 `json:"download_ms"`
	SinkWriteStartedMS float64 `json:"sink_write_started_ms"`
	SinkWriteMS        float64 `json:"sink_write_ms"`
	BytesWritten       int     `json:"bytes_written"`
	CumulativeBytes    int64   `json:"cumulative_bytes"`
	ErrorClass         string  `json:"error_class,omitempty"`
	RetryAfter         string  `json:"retry_after,omitempty"`
}
type StreamReport struct {
	URL                  string          `json:"url"`
	Client               string          `json:"client"`
	Scope                string          `json:"scope"`
	StartedAt            time.Time       `json:"started_at"`
	EndedAt              time.Time       `json:"ended_at"`
	ElapsedMS            float64         `json:"elapsed_ms"`
	EndList              bool            `json:"endlist"`
	MediaDurationSeconds float64         `json:"media_duration_seconds"`
	SegmentCount         int             `json:"segment_count"`
	CompletedSegments    int             `json:"completed_segments"`
	DeclaredBytes        int64           `json:"declared_bytes"`
	BytesDownloaded      int64           `json:"bytes_downloaded"` // media bytes; manifest is separate
	BytesWritten         int64           `json:"bytes_written"`
	StopReason           string          `json:"stop_reason"`
	Manifest             Request         `json:"manifest"`
	Segments             []StreamSegment `json:"segments"`
}

func streamDefaults(cfg StreamConfig) StreamConfig {
	if cfg.Client == "" {
		cfg.Client = "stdlib"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return cfg
}
func ValidateStreamConfig(cfg StreamConfig) error {
	cfg = streamDefaults(cfg)
	if _, err := proxytransport.Parse(cfg.ProxyURL); err != nil {
		return err
	}
	if _, err := mediaURL(cfg.URL); err != nil {
		return err
	}
	if cfg.Client != "stdlib" && cfg.Client != "chrome_152" {
		return errors.New("client must be stdlib or chrome_152")
	}
	if cfg.Timeout <= 0 || cfg.Timeout > 30*time.Second {
		return errors.New("request timeout must be positive and at most 30s")
	}
	return nil
}

// StreamVOD transfers one finite TS byte-range playlist sequentially. TS sync
// validation is not decoder validation. The caller owns the sink and must unblock
// its Write on ctx cancellation (for example by closing decoder stdin/killing its
// context-bound process). Sink writes provide backpressure; no media is persisted.
func StreamVOD(ctx context.Context, cfg StreamConfig, sink io.Writer) (StreamReport, error) {
	return streamVOD(ctx, cfg, sink, cfg.RootCAs)
}
func streamVOD(ctx context.Context, cfg StreamConfig, sink io.Writer, roots *x509.CertPool) (report StreamReport, err error) {
	if ctx == nil || sink == nil {
		return report, errors.New("context and media sink are required")
	}
	cfg = streamDefaults(cfg)
	if err := ValidateStreamConfig(cfg); err != nil {
		return report, err
	}
	u, _ := mediaURL(cfg.URL)
	start := time.Now()
	report = StreamReport{URL: redactedURL(u), Client: cfg.Client, StartedAt: start.UTC(), Scope: "finite_ts_byterange_transfer; TS sync validated, decoding owned by the caller"}
	defer func() {
		report.EndedAt = time.Now().UTC()
		report.ElapsedMS = float64(time.Since(start)) / float64(time.Millisecond)
	}()
	fail := func(class string) error {
		report.StopReason = class
		if ctx.Err() != nil {
			report.StopReason = "cancelled"
			return ctx.Err()
		}
		if class == "timeout" {
			return fmt.Errorf("media request timed out: %w", context.DeadlineExceeded)
		}
		return errors.New("media stream stopped: " + class)
	}
	if ctx.Err() != nil {
		return report, fail("cancelled")
	}
	fetch, closeClient, err := newFetcherWithPrefixLimit(Config{Client: cfg.Client, Timeout: cfg.Timeout, ProxyURL: cfg.ProxyURL}, roots, maxStreamSegment, u)
	if err != nil {
		return report, fail("client_initialization")
	}
	defer closeClient()
	read := func(target *url.URL, full bool, selected *byteRange) responseData {
		requestCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		return fetch(requestCtx, target, full, selected)
	}
	manifest := read(u, true, nil)
	manifest.request.Stage = "manifest"
	report.Manifest = manifest.request
	if manifest.class != "" {
		return report, fail(streamHTTPClass(manifest))
	}
	plan, class := parseStreamPlaylist(manifest.body, manifest.finalURL, u)
	manifest.body = nil
	if class != "" {
		return report, fail(class)
	}
	report.EndList, report.SegmentCount = true, len(plan)
	for _, part := range plan {
		report.MediaDurationSeconds += part.duration
		report.DeclaredBytes += part.rangeSpec.length
	}
	for i, part := range plan {
		if ctx.Err() != nil {
			return report, fail("cancelled")
		}
		entry := StreamSegment{Index: i + 1, URL: redactedURL(part.url), StartSeconds: part.start, DurationSeconds: part.duration, Offset: part.rangeSpec.start, Length: part.rangeSpec.length, DownloadStartedMS: float64(time.Since(start)) / float64(time.Millisecond)}
		response := read(part.url, false, &part.rangeSpec)
		entry.Status, entry.Bytes, entry.DownloadMS = response.request.Status, response.request.BytesRead, response.request.ElapsedMS
		entry.RequestedRange, entry.ContentRange, entry.RetryAfter = response.request.RequestedRange, response.request.ContentRange, response.request.RetryAfter
		report.BytesDownloaded += int64(entry.Bytes)
		entry.CumulativeBytes = report.BytesDownloaded
		class = streamHTTPClass(response)
		if class == "" && !validTSPackets(response.body) {
			class = "invalid_ts"
		}
		if class != "" {
			entry.ErrorClass = class
			report.Segments = append(report.Segments, entry)
			return report, fail(class)
		}
		digest := sha256.Sum256(response.body)
		entry.SHA256 = hex.EncodeToString(digest[:])
		if ctx.Err() != nil {
			entry.ErrorClass = "cancelled"
			report.Segments = append(report.Segments, entry)
			return report, fail("cancelled")
		}
		writeStarted := time.Now()
		entry.SinkWriteStartedMS = float64(time.Since(start)) / float64(time.Millisecond)
		n, writeErr := sink.Write(response.body)
		entry.SinkWriteMS = float64(time.Since(writeStarted)) / float64(time.Millisecond)
		entry.BytesWritten = max(0, min(n, len(response.body)))
		report.BytesWritten += int64(entry.BytesWritten)
		if writeErr != nil || n != len(response.body) {
			class = "sink_write_failed"
		}
		response.body = nil
		if ctx.Err() != nil {
			class = "cancelled"
		}
		entry.ErrorClass = class
		report.Segments = append(report.Segments, entry)
		if class != "" {
			return report, fail(class)
		}
		report.CompletedSegments++
	}
	report.StopReason = "complete"
	return report, nil
}

func streamHTTPClass(response responseData) string {
	if response.request.Status == 401 || response.request.Status == 403 || response.request.Status == 429 {
		return "restricted"
	}
	return response.class
}
func validTSPackets(data []byte) bool {
	if len(data) < 188 || len(data)%188 != 0 {
		return false
	}
	for offset := 0; offset < len(data); offset += 188 {
		if data[offset] != 0x47 {
			return false
		}
	}
	return true
}

type streamPart struct {
	url             *url.URL
	rangeSpec       byteRange
	start, duration float64
}

// Preflight the complete manifest before downloading the first media byte.
func parseStreamPlaylist(body []byte, base, origin *url.URL) ([]streamPart, string) {
	if isHTML(body, "") {
		return nil, "page_url"
	}
	if !utf8.Valid(body) {
		return nil, "invalid_manifest"
	}
	lines := strings.Split(string(bytes.TrimSpace(body)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, "invalid_manifest"
	}
	var plan []streamPart
	var duration, totalDuration float64
	var rangeText string
	var totalBytes int64
	var ended bool
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"), strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			return nil, "unsupported_master"
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			return nil, "unsupported_init_map"
		case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
			return nil, "unsupported_discontinuity"
		case strings.HasPrefix(line, "#EXT-X-PART"), strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:"), strings.HasPrefix(line, "#EXT-X-SERVER-CONTROL:"), strings.HasPrefix(line, "#EXT-X-SKIP:"), strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:"):
			return nil, "unsupported_llhls"
		case strings.HasPrefix(line, "#EXT-X-KEY:"), strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			_, value, _ := strings.Cut(line, ":")
			attributes, ok := hlsAttributes(value)
			if !ok || attributes["METHOD"] != "NONE" {
				return nil, "unsupported_encryption"
			}
		case line == "#EXT-X-ENDLIST":
			if ended || duration != 0 || rangeText != "" {
				return nil, "invalid_manifest"
			}
			ended = true
		case strings.HasPrefix(line, "#EXTINF:"):
			if ended || duration != 0 {
				return nil, "invalid_manifest"
			}
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			var err error
			duration, err = strconv.ParseFloat(value, 64)
			if err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
				return nil, "invalid_manifest"
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			if ended || rangeText != "" {
				return nil, "invalid_manifest"
			}
			rangeText = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			if rangeText == "" {
				return nil, "invalid_manifest"
			}
		case strings.HasPrefix(line, "#EXT-X-VERSION:"), strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"), strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"), line == "#EXT-X-INDEPENDENT-SEGMENTS", line == "#EXT-X-PLAYLIST-TYPE:VOD":
			continue
		case strings.HasPrefix(line, "#EXT"):
			return nil, "unsupported_tag"
		case strings.HasPrefix(line, "#"):
			continue
		default:
			if ended || duration == 0 || rangeText == "" {
				return nil, "unsupported_non_byterange_media"
			}
			reference, err := url.Parse(line)
			if err != nil {
				return nil, "unsupported_url"
			}
			target := base.ResolveReference(reference)
			if _, err := mediaURL(target.String()); err != nil {
				return nil, "unsupported_url"
			}
			if !sameMediaOrigin(origin, target) {
				return nil, "unsupported_origin"
			}
			lengthText, offsetText, explicit := strings.Cut(rangeText, "@")
			length, err := positiveInteger(lengthText)
			if err != nil {
				return nil, "invalid_manifest"
			}
			if length > maxStreamSegment {
				return nil, "segment_limit"
			}
			var offset int64
			if explicit {
				if offsetText == "" || strings.Trim(offsetText, "0123456789") != "" {
					return nil, "invalid_manifest"
				}
				offset, err = strconv.ParseInt(offsetText, 10, 64)
				if err != nil {
					return nil, "invalid_manifest"
				}
			} else {
				if len(plan) == 0 || plan[len(plan)-1].url.String() != target.String() {
					return nil, "unsupported_implicit_byterange"
				}
				previous := plan[len(plan)-1].rangeSpec
				offset = previous.start + previous.length
			}
			if offset > math.MaxInt64-length {
				return nil, "invalid_manifest"
			}
			if length < 188 || length%188 != 0 {
				return nil, "unsupported_ts_length"
			}
			if len(plan) >= 64 || totalBytes+length > maxStreamBytes || totalDuration+duration > 60 {
				return nil, "stream_limit"
			}
			plan = append(plan, streamPart{target, byteRange{offset, length}, totalDuration, duration})
			totalBytes += length
			totalDuration += duration
			duration, rangeText = 0, ""
		}
	}
	if !ended {
		return nil, "unsupported_live"
	}
	if len(plan) == 0 || duration != 0 || rangeText != "" {
		return nil, "invalid_manifest"
	}
	return plan, ""
}
