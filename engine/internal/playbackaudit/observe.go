// Package playbackaudit records decoder evidence. Frame-log arrival gaps are
// observations of a subprocess pipe, not player presentation stalls.
package playbackaudit

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxProgressSamples = 512
const maxDiagnostics = 20
const maxDiagnosticBytes = 1024

type ProgressSample struct {
	AfterMS   float64  `json:"after_ms"`
	Frame     int      `json:"frame"`
	OutTimeUS *int64   `json:"out_time_us,omitempty"`
	Speed     *float64 `json:"speed,omitempty"`
	State     string   `json:"state"`
}

type Diagnostic struct {
	Class string `json:"class"`
	Line  string `json:"line"`
}

type DecodeStats struct {
	VideoFrames                 int              `json:"video_frames"`
	AudioFrames                 int              `json:"audio_frames"`
	AudioSamples                int64            `json:"audio_samples"`
	ProgressEnd                 bool             `json:"progress_end"`
	ProgressFrames              int              `json:"progress_frames"`
	DecodeError                 bool             `json:"decode_error"`
	FirstVideoPTS               *float64         `json:"first_video_pts,omitempty"`
	LastVideoPTS                *float64         `json:"last_video_pts,omitempty"`
	VideoPTSSpanSeconds         float64          `json:"video_pts_span_seconds"`
	LastFrameDurationSeconds    *float64         `json:"last_frame_duration_seconds,omitempty"`
	DecodedVideoDurationSeconds *float64         `json:"decoded_video_duration_seconds,omitempty"`
	AudioDurationSeconds        float64          `json:"audio_duration_seconds"`
	AudioFirstPTS               *float64         `json:"audio_first_pts,omitempty"`
	AudioLastPTS                *float64         `json:"audio_last_pts,omitempty"`
	AudioRates                  []int            `json:"audio_rates,omitempty"`
	Width                       int              `json:"width,omitempty"`
	Height                      int              `json:"height,omitempty"`
	PixelFormat                 string           `json:"pixel_format,omitempty"`
	InputVideoCodec             string           `json:"input_video_codec,omitempty"`
	InputAudioCodec             string           `json:"input_audio_codec,omitempty"`
	FirstVideoFrameMS           float64          `json:"first_video_frame_ms"`
	MaxVideoFrameGapMS          float64          `json:"max_video_frame_gap_ms"`
	LastProgressTimeSeconds     *float64         `json:"last_progress_time_seconds,omitempty"`
	ProgressSamples             []ProgressSample `json:"progress_samples,omitempty"`
	ProgressSamplesDropped      int              `json:"progress_samples_dropped"`
	Warnings                    int              `json:"warnings"`
	Errors                      int              `json:"errors"`
	ParseErrors                 int              `json:"parse_errors"`
	VideoSequenceErrors         int              `json:"video_sequence_errors"`
	AudioSequenceErrors         int              `json:"audio_sequence_errors"`
	ErrorClasses                []string         `json:"error_classes,omitempty"`
	Diagnostics                 []Diagnostic     `json:"diagnostics,omitempty"`
}

type pendingProgress struct {
	frame            int
	hasFrame         bool
	outTimeUS        *int64
	hasPreferredTime bool
	speed            *float64
}

type observer struct {
	mu               sync.Mutex
	start            time.Time
	stats            DecodeStats
	inputMetadata    bool
	lastVideoArrival time.Time
	pending          pendingProgress
	videoSequence    frameSequence
	audioSequence    frameSequence
}

type frameSequence struct {
	seen bool
	last int
}

var showFields = regexp.MustCompile(`(?:^|\s)([A-Za-z_][A-Za-z0-9_]*):\s*([^\s]+)`)
var inputStream = regexp.MustCompile(`Stream #0:[0-9]+(?:\[[^\]]*\])?(?:\([^)]*\))?:\s*(Video|Audio):\s*([A-Za-z0-9_.-]+)`)

// av_log calls from audio and video filters can share one physical stderr line.
// In particular, an audio core can directly follow "color_range:tv" without its
// own prefix or whitespace. Only the complete core establishes the frame kind;
// do not infer it from whichever component prefix happens to precede the line.
var frameStarts = regexp.MustCompile(`n:\s*\S+\s+pts:`)
var isolatedFrameMarker = regexp.MustCompile(`(?:^|\s)n:`)
var videoCore = regexp.MustCompile(`^n:\s*\S+\s+pts:\s*\S+\s+pts_time:\s*\S+\s+(?:duration:\s*\S+\s+duration_time:\s*\S+\s+)?(?:pos:\s*\S+\s+)?fmt:\s*\S+\s+(?:cl:\s*\S+\s+)?sar:\s*\S+\s+s:\s*\S+\s+i:\s*\S+\s+iskey:\s*\S+\s+type:\s*\S+(?:\s|$)`)
var audioCore = regexp.MustCompile(`^n:\s*\S+\s+pts:\s*\S+\s+pts_time:\s*\S+\s+(?:pos:\s*\S+\s+)?fmt:\s*\S+\s+channels:\s*\S+\s+(?:chlayout|channel_layout):\s*\S+\s+rate:\s*\S+\s+nb_samples:\s*\S+(?:\s|$)`)

func newObserver(start time.Time) *observer { return &observer{start: start} }

func (o *observer) stderr(line string) { o.stderrAt(line, time.Now()) }

func (o *observer) stderrAt(line string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	line, severity := stripLogSeverity(line)
	class := diagnosticClass(line)
	if severity == "error" || severity == "fatal" || severity == "panic" {
		if class == "info" || class == "warning" {
			class = "decoder_log_error"
		}
	} else if severity == "warning" && class == "info" {
		class = "warning"
	}
	if severity == "warning" || class == "warning" {
		o.stats.Warnings++
	}
	if class != "info" && class != "warning" {
		o.stats.DecodeError = true
		o.stats.Errors++
		found := false
		for _, existing := range o.stats.ErrorClasses {
			if existing == class {
				found = true
				break
			}
		}
		if !found {
			o.stats.ErrorClasses = append(o.stats.ErrorClasses, class)
		}
	}
	if strings.HasPrefix(line, "Input #0,") {
		o.inputMetadata = true
	}
	if strings.HasPrefix(line, "Output #") || strings.HasPrefix(line, "Stream mapping:") || (strings.HasPrefix(line, "Input #") && !strings.HasPrefix(line, "Input #0,")) {
		o.inputMetadata = false
	}
	if o.inputMetadata {
		match := inputStream.FindStringSubmatch(line)
		if len(match) == 3 {
			if match[1] == "Video" && o.stats.InputVideoCodec == "" {
				o.stats.InputVideoCodec = match[2]
			}
			if match[1] == "Audio" && o.stats.InputAudioCodec == "" {
				o.stats.InputAudioCodec = match[2]
			}
		}
	}
	starts := frameStarts.FindAllStringIndex(line, -1)
	if len(starts) > 0 {
		if class != "info" {
			o.diagnostic(class, line)
		}
		for i, start := range starts {
			end := len(line)
			if i+1 < len(starts) {
				end = starts[i+1][0]
			}
			o.frameCore(line[start[0]:end], now)
		}
		return
	}
	if strings.Contains(line, "showinfo") && isolatedFrameMarker.MatchString(line) {
		o.parseError("incomplete decoder frame header")
		return
	}
	o.diagnostic(class, line)
}

func (o *observer) frameCore(fragment string, now time.Time) {
	core, audio := audioCore.FindString(fragment), true
	if core == "" {
		core, audio = videoCore.FindString(fragment), false
	}
	if core == "" {
		o.parseError("incomplete or interleaved decoder frame core")
		return
	}
	fields := make(map[string]string)
	for _, match := range showFields.FindAllStringSubmatch(core, -1) {
		if _, duplicate := fields[match[1]]; duplicate {
			o.parseError("duplicate decoder frame field")
			return
		}
		fields[match[1]] = match[2]
	}
	if _, err := strconv.ParseInt(fields["pts"], 10, 64); err != nil {
		o.parseError("invalid decoder frame timestamp")
		return
	}
	if audio {
		if _, ok := positiveInt(fields["channels"]); !ok {
			o.parseError("invalid audio channel count")
			return
		}
		o.audioFrame(fields)
	} else {
		o.videoFrame(fields, now)
	}
}

func (o *observer) videoFrame(fields map[string]string, now time.Time) {
	index, ok := nonnegativeInt(fields["n"])
	pts, validPTS := finiteFloat(fields["pts_time"])
	if !ok || !validPTS {
		o.parseError("invalid video frame index or PTS")
		return
	}
	var duration *float64
	if text, exists := fields["duration_time"]; exists && text != "N/A" && text != "NOPTS" {
		value, ok := finiteFloat(text)
		if !ok || value < 0 {
			o.parseError("invalid video frame duration")
			return
		}
		duration = &value
	}
	width, height := o.stats.Width, o.stats.Height
	if text, exists := fields["s"]; exists {
		left, right, found := strings.Cut(text, "x")
		w, wOK := positiveInt(left)
		h, hOK := positiveInt(right)
		if !found || !wOK || !hOK {
			o.parseError("invalid video frame dimensions")
			return
		}
		width, height = w, h
	}
	span := float64(0)
	if o.stats.FirstVideoPTS != nil {
		span = pts - *o.stats.FirstVideoPTS
		if math.IsInf(span, 0) || math.IsNaN(span) {
			o.parseError("video timestamp span overflow")
			return
		}
	}
	if o.stats.VideoFrames == 0 {
		o.stats.FirstVideoPTS = copyFloat(&pts)
		o.stats.FirstVideoFrameMS = max(0, float64(now.Sub(o.start))/float64(time.Millisecond))
	} else {
		gap := float64(now.Sub(o.lastVideoArrival)) / float64(time.Millisecond)
		o.stats.MaxVideoFrameGapMS = max(o.stats.MaxVideoFrameGapMS, gap)
	}
	if !o.recordFrameIndex(index, &o.videoSequence, "video") {
		o.stats.VideoSequenceErrors++
	}
	o.lastVideoArrival = now
	o.stats.VideoFrames++
	o.stats.LastVideoPTS = copyFloat(&pts)
	o.stats.LastFrameDurationSeconds = copyFloat(duration)
	o.stats.VideoPTSSpanSeconds = span
	o.stats.DecodedVideoDurationSeconds = nil
	if duration != nil {
		value := o.stats.VideoPTSSpanSeconds + *duration
		if value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value) {
			o.stats.DecodedVideoDurationSeconds = &value
		}
	}
	o.stats.Width, o.stats.Height = width, height
	if format, exists := fields["fmt"]; exists {
		o.stats.PixelFormat = truncateLine(format, 128)
	}
}

func (o *observer) audioFrame(fields map[string]string) {
	index, ok := nonnegativeInt(fields["n"])
	pts, validPTS := finiteFloat(fields["pts_time"])
	rate, validRate := positiveInt(fields["rate"])
	samples, err := strconv.ParseInt(fields["nb_samples"], 10, 64)
	if !ok || !validPTS || !validRate || err != nil || samples <= 0 || o.stats.AudioSamples > math.MaxInt64-samples {
		o.parseError("invalid audio frame fields or sample-count overflow")
		return
	}
	duration := o.stats.AudioDurationSeconds + float64(samples)/float64(rate)
	if math.IsNaN(duration) || math.IsInf(duration, 0) {
		o.parseError("audio duration overflow")
		return
	}
	if o.stats.AudioFrames == 0 {
		o.stats.AudioFirstPTS = copyFloat(&pts)
	}
	if !o.recordFrameIndex(index, &o.audioSequence, "audio") {
		o.stats.AudioSequenceErrors++
	}
	o.stats.AudioLastPTS = copyFloat(&pts)
	o.stats.AudioFrames++
	o.stats.AudioSamples += samples
	o.stats.AudioDurationSeconds = duration
	for _, known := range o.stats.AudioRates {
		if known == rate {
			return
		}
	}
	o.stats.AudioRates = append(o.stats.AudioRates, rate)
	sort.Ints(o.stats.AudioRates)
}

// Count observed frame records, not a guessed last-index-plus-one. A sequence
// anomaly makes the trace incomplete even when every individual field parses.
func (o *observer) recordFrameIndex(index int, sequence *frameSequence, kind string) bool {
	valid := (!sequence.seen && index == 0) || (sequence.seen && index > sequence.last && index-sequence.last == 1)
	sequence.seen, sequence.last = true, index
	if !valid {
		o.parseError("nonconsecutive " + kind + " frame index")
	}
	return valid
}

// FFmpeg's level+info logging may put the severity before or after a component
// prefix. Only leading bracket groups are inspected, not message body text.
func stripLogSeverity(line string) (string, string) {
	for offset := 0; offset < len(line) && line[offset] == '['; {
		closing := strings.IndexByte(line[offset:], ']')
		if closing < 0 {
			break
		}
		end := offset + closing + 1
		level := line[offset+1 : end-1]
		switch level {
		case "quiet", "panic", "fatal", "error", "warning", "info", "verbose", "debug", "trace":
			return strings.TrimSpace(line[:offset] + strings.TrimLeft(line[end:], " \t")), level
		}
		offset = end
		for offset < len(line) && (line[offset] == ' ' || line[offset] == '\t') {
			offset++
		}
	}
	return line, ""
}

func (o *observer) progress(line string) { o.progressAt(line, time.Now()) }

func (o *observer) progressAt(line string, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok {
		return
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	switch key {
	case "frame":
		frame, ok := nonnegativeInt(value)
		if !ok {
			o.parseError("invalid progress frame count")
			return
		}
		o.pending.frame, o.pending.hasFrame = frame, true
	case "out_time_us", "out_time_ms":
		// FFmpeg's historical out_time_ms key also carries microseconds.
		// Prefer the correctly named key when both are present in one block.
		if key == "out_time_ms" && o.pending.hasPreferredTime {
			return
		}
		if key == "out_time_us" {
			o.pending.hasPreferredTime = true
		}
		if value == "N/A" {
			o.pending.outTimeUS = nil
			return
		}
		microseconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			o.parseError("invalid progress timestamp")
			return
		}
		o.pending.outTimeUS = &microseconds
	case "speed":
		if value == "N/A" {
			o.pending.speed = nil
			return
		}
		speed, ok := finiteFloat(strings.TrimSuffix(value, "x"))
		if !ok || speed < 0 {
			o.parseError("invalid progress speed")
			return
		}
		o.pending.speed = &speed
	case "progress":
		if value != "continue" && value != "end" {
			o.parseError("invalid progress state")
			o.pending = pendingProgress{}
			return
		}
		if o.pending.hasFrame {
			o.stats.ProgressFrames = o.pending.frame
		}
		if o.pending.outTimeUS != nil {
			seconds := float64(*o.pending.outTimeUS) / 1e6
			o.stats.LastProgressTimeSeconds = &seconds
		}
		if value == "end" {
			o.stats.ProgressEnd = true
		}
		sample := ProgressSample{AfterMS: max(0, float64(now.Sub(o.start))/float64(time.Millisecond)), Frame: o.pending.frame, OutTimeUS: copyInt64(o.pending.outTimeUS), Speed: copyFloat(o.pending.speed), State: value}
		if len(o.stats.ProgressSamples) == maxProgressSamples {
			copy(o.stats.ProgressSamples, o.stats.ProgressSamples[1:])
			o.stats.ProgressSamples[len(o.stats.ProgressSamples)-1] = sample
			o.stats.ProgressSamplesDropped++
		} else {
			o.stats.ProgressSamples = append(o.stats.ProgressSamples, sample)
		}
		o.pending = pendingProgress{}
	}
}

func (o *observer) snapshot() DecodeStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	stats := o.stats
	stats.FirstVideoPTS = copyFloat(stats.FirstVideoPTS)
	stats.LastVideoPTS = copyFloat(stats.LastVideoPTS)
	stats.LastFrameDurationSeconds = copyFloat(stats.LastFrameDurationSeconds)
	stats.DecodedVideoDurationSeconds = copyFloat(stats.DecodedVideoDurationSeconds)
	stats.AudioFirstPTS = copyFloat(stats.AudioFirstPTS)
	stats.AudioLastPTS = copyFloat(stats.AudioLastPTS)
	stats.LastProgressTimeSeconds = copyFloat(stats.LastProgressTimeSeconds)
	stats.AudioRates = append([]int(nil), stats.AudioRates...)
	stats.ErrorClasses = append([]string(nil), stats.ErrorClasses...)
	stats.Diagnostics = append([]Diagnostic(nil), stats.Diagnostics...)
	stats.ProgressSamples = append([]ProgressSample(nil), stats.ProgressSamples...)
	for i := range stats.ProgressSamples {
		stats.ProgressSamples[i].OutTimeUS = copyInt64(stats.ProgressSamples[i].OutTimeUS)
		stats.ProgressSamples[i].Speed = copyFloat(stats.ProgressSamples[i].Speed)
	}
	return stats
}

func (o *observer) parseError(message string) {
	o.stats.ParseErrors++
	o.diagnostic("parse_error", message)
}

func (o *observer) diagnostic(class, line string) {
	entry := Diagnostic{Class: class, Line: truncateLine(line, maxDiagnosticBytes)}
	if len(o.stats.Diagnostics) == maxDiagnostics {
		copy(o.stats.Diagnostics, o.stats.Diagnostics[1:])
		o.stats.Diagnostics[len(o.stats.Diagnostics)-1] = entry
	} else {
		o.stats.Diagnostics = append(o.stats.Diagnostics, entry)
	}
}

func diagnosticClass(line string) string {
	lower := strings.ToLower(line)
	for _, pattern := range []string{"error while decoding", "error decoding", "error submitting packet to decoder", "error sending packet to decoder", "error splitting the input into nal units", "invalid nal unit", "corrupt decoded frame", "packet corrupt", "decode_slice_header error"} {
		if strings.Contains(lower, pattern) {
			return "decode_error"
		}
	}
	for _, pattern := range []string{"invalid data found when processing input", "could not find codec parameters", "decoder not found", "unknown decoder"} {
		if strings.Contains(lower, pattern) {
			return "input_error"
		}
	}
	for _, pattern := range []string{"conversion failed!", "error initializing filters", "error reinitializing filters", "failed to inject frame into filter network", "error writing trailer", "error submitting a packet to the muxer"} {
		if strings.Contains(lower, pattern) {
			return "pipeline_error"
		}
	}
	if strings.Contains(lower, "[warning]") || strings.Contains(lower, "warning:") || strings.Contains(lower, "deprecated pixel format") || strings.Contains(lower, "non monotonically increasing dts") || strings.Contains(lower, "non-monotonous dts") {
		return "warning"
	}
	return "info"
}

func finiteFloat(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}
func nonnegativeInt(value string) (int, bool) {
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil && parsed >= 0
}
func positiveInt(value string) (int, bool) {
	parsed, ok := nonnegativeInt(value)
	return parsed, ok && parsed > 0
}
func copyFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func copyInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func truncateLine(line string, limit int) string {
	if len(line) <= limit {
		return line
	}
	line = line[:limit]
	for !utf8.ValidString(line) && len(line) > 0 {
		line = line[:len(line)-1]
	}
	return line
}
