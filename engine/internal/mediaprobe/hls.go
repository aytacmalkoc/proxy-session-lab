package mediaprobe

import (
	"bytes"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type playlistSelection struct {
	variant, segment *url.URL
	segmentRange     *byteRange
}

func parsePlaylist(body []byte, base *url.URL) (playlistSelection, string) {
	var result playlistSelection
	if isHTML(body, "") {
		return result, "page_url"
	}
	if !utf8.Valid(body) {
		return result, "invalid_manifest"
	}
	lines := strings.Split(string(bytes.TrimSpace(body)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return result, "invalid_manifest"
	}
	type variant struct {
		bandwidth uint64
		url       *url.URL
	}
	var variants []variant
	var bandwidth uint64
	var variantPending, segmentPending bool
	var rangeText string
	var previousURL string
	var previousRange *byteRange
	resolve := func(value string) (*url.URL, bool) {
		reference, err := url.Parse(value)
		if err != nil {
			return nil, false
		}
		u := base.ResolveReference(reference)
		_, err = mediaURL(u.String())
		return u, err == nil
	}
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-PART:"), strings.HasPrefix(line, "#EXT-X-PART-INF:"), strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:"), strings.HasPrefix(line, "#EXT-X-SERVER-CONTROL:"), strings.HasPrefix(line, "#EXT-X-SKIP:"), strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:"):
			return result, "unsupported_llhls"
		case strings.HasPrefix(line, "#EXT-X-KEY:"), strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			_, value, _ := strings.Cut(line, ":")
			attributes, ok := hlsAttributes(value)
			if !ok {
				return result, "invalid_manifest"
			}
			if attributes["METHOD"] != "NONE" {
				return result, "unsupported_encryption"
			}
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if variantPending || segmentPending || rangeText != "" {
				return result, "invalid_manifest"
			}
			attributes, ok := hlsAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			var err error
			bandwidth, err = strconv.ParseUint(attributes["BANDWIDTH"], 10, 64)
			if !ok || err != nil || bandwidth == 0 {
				return result, "invalid_manifest"
			}
			variantPending = true
		case strings.HasPrefix(line, "#EXTINF:"):
			if segmentPending || variantPending {
				return result, "invalid_manifest"
			}
			durationText, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			duration, err := strconv.ParseFloat(durationText, 64)
			if err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
				return result, "invalid_manifest"
			}
			segmentPending = true
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			if variantPending || rangeText != "" {
				return result, "invalid_manifest"
			}
			rangeText = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			// This access probe deliberately does not fetch or validate init
			// data. A MAP URI is never mistaken for an ordinary media segment.
			continue
		case strings.HasPrefix(line, "#"):
			continue
		default:
			u, ok := resolve(line)
			if !ok {
				return result, "unsupported_url"
			}
			if variantPending {
				variants = append(variants, variant{bandwidth, u})
				variantPending = false
				continue
			}
			if !segmentPending {
				return result, "invalid_manifest"
			}
			var selected *byteRange
			if rangeText != "" {
				lengthText, offsetText, explicit := strings.Cut(rangeText, "@")
				length, err := positiveInteger(lengthText)
				if err != nil {
					return result, "invalid_manifest"
				}
				var offset int64
				if explicit {
					if offsetText == "0" {
						offset = 0
					} else {
						offset, err = positiveInteger(offsetText)
						if err != nil {
							return result, "invalid_manifest"
						}
					}
				} else {
					if previousRange == nil || previousURL != u.String() {
						return result, "unsupported_implicit_byterange"
					}
					offset = previousRange.start + previousRange.length
				}
				if offset > math.MaxInt64-length {
					return result, "invalid_manifest"
				}
				selected = &byteRange{offset, length}
			}
			if result.segment == nil {
				result.segment, result.segmentRange = u, selected
			}
			previousURL, previousRange = u.String(), selected
			segmentPending, rangeText = false, ""
		}
	}
	if variantPending || segmentPending || rangeText != "" {
		return result, "invalid_manifest"
	}
	if len(variants) > 0 {
		if result.segment != nil {
			return result, "invalid_manifest"
		}
		sort.Slice(variants, func(i, j int) bool {
			if variants[i].bandwidth == variants[j].bandwidth {
				return variants[i].url.String() < variants[j].url.String()
			}
			return variants[i].bandwidth < variants[j].bandwidth
		})
		result.variant = variants[0].url
	} else if result.segment == nil {
		return result, "invalid_manifest"
	}
	return result, ""
}

func positiveInteger(text string) (int64, error) {
	if text == "" || strings.Trim(text, "0123456789") != "" {
		return 0, strconv.ErrSyntax
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 {
		return 0, strconv.ErrSyntax
	}
	return value, nil
}

// HLS quoted attributes can contain commas (for example CODECS and signed URIs).
func hlsAttributes(value string) (map[string]string, bool) {
	result := make(map[string]string)
	for value != "" {
		key, rest, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, false
		}
		key = strings.TrimSpace(key)
		if _, exists := result[key]; exists {
			return nil, false
		}
		var part string
		if strings.HasPrefix(rest, `"`) {
			part, rest, ok = strings.Cut(rest[1:], `"`)
			if !ok || (rest != "" && !strings.HasPrefix(rest, ",")) {
				return nil, false
			}
			value = strings.TrimPrefix(rest, ",")
		} else {
			part, value, _ = strings.Cut(rest, ",")
		}
		result[key] = strings.TrimSpace(part)
	}
	return result, true
}
