package playbackaudit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var observedStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func videoLine(n int, pts, duration string) string {
	extra := ""
	if duration != "" {
		extra = " duration: 3000 duration_time:" + duration
	}
	return fmt.Sprintf("[Parsed_showinfo_0 @ 0x1234] n:%4d pts: 126000 pts_time:%s%s fmt:yuv420p cl:left sar:1/1 s:1280x720 i:P iskey:1 type:I", n, pts, extra)
}
func audioLine(n int, pts string, samples, rate int) string {
	return fmt.Sprintf("[Parsed_ashowinfo_0 @ 0xabcd] n:%d pts:67200 pts_time:%s fmt:fltp channels:2 chlayout:stereo rate:%d nb_samples:%d", n, pts, rate, samples)
}

func TestObserverReadsInputMetadataAndRealisticFrameLines(t *testing.T) {
	o := newObserver(observedStart)
	for _, line := range []string{
		"Input #0, mpegts, from 'pipe:0':",
		"  Duration: N/A, start: 1.400000, bitrate: N/A",
		"  Stream #0:0[0x100]: Video: h264 (Main) ([27][0][0][0] / 0x001B), yuv420p, 1280x720, 30 fps",
		"  Stream #0:1[0x101](eng): Audio: aac (LC), 48000 Hz, stereo, fltp",
		"Stream mapping:",
		"  Stream #0:0 -> #0:0 (h264 (native) -> wrapped_avframe (native))",
		"Output #0, null, to '/dev/null':",
		"  Stream #0:0: Video: wrapped_avframe, yuv420p, 1280x720",
		"  Stream #0:1: Audio: pcm_s16le, 48000 Hz, stereo",
	} {
		o.stderrAt(line, observedStart)
	}
	o.stderrAt(videoLine(0, "1.4", "0.0333333"), observedStart.Add(250*time.Millisecond))
	o.stderrAt(audioLine(0, "1.4", 1024, 48000), observedStart.Add(260*time.Millisecond))
	o.stderrAt(videoLine(1, "1.4333333", "0.0333333"), observedStart.Add(300*time.Millisecond))
	o.stderrAt(audioLine(1, "1.4213333", 1024, 48000), observedStart.Add(310*time.Millisecond))
	s := o.snapshot()
	if s.VideoFrames != 2 || s.AudioFrames != 2 || s.AudioSamples != 2048 || s.ParseErrors != 0 || s.DecodeError {
		t.Fatal(s)
	}
	if s.InputVideoCodec != "h264" || s.InputAudioCodec != "aac" || s.Width != 1280 || s.Height != 720 || s.PixelFormat != "yuv420p" {
		t.Fatal(s)
	}
	if s.FirstVideoPTS == nil || *s.FirstVideoPTS != 1.4 || s.LastVideoPTS == nil || *s.LastVideoPTS != 1.4333333 {
		t.Fatal(s)
	}
	if s.DecodedVideoDurationSeconds == nil || math.Abs(*s.DecodedVideoDurationSeconds-.0666666) > 1e-9 {
		t.Fatal(s)
	}
	if math.Abs(s.AudioDurationSeconds-2048.0/48000) > 1e-12 || len(s.AudioRates) != 1 || s.AudioRates[0] != 48000 {
		t.Fatal(s)
	}
	if s.FirstVideoFrameMS != 250 || s.MaxVideoFrameGapMS != 50 {
		t.Fatal("arrival timing mismatch", s)
	}
}

func TestMissingLastFrameDurationRemainsUnknown(t *testing.T) {
	o := newObserver(observedStart)
	o.stderrAt(videoLine(0, "10", "0.04"), observedStart)
	o.stderrAt(videoLine(1, "10.04", ""), observedStart.Add(time.Second))
	s := o.snapshot()
	if s.VideoFrames != 2 || s.LastFrameDurationSeconds != nil || s.DecodedVideoDurationSeconds != nil || math.Abs(s.VideoPTSSpanSeconds-.04) > 1e-9 {
		t.Fatal(s)
	}
}

func TestProgressMicrosecondsEndAndLegacyName(t *testing.T) {
	o := newObserver(observedStart)
	for _, line := range []string{"frame= 30", "fps=29.7", "out_time_us=1500000", "out_time_ms=9999999", "speed=1.25x", "progress=continue"} {
		o.progressAt(line, observedStart.Add(time.Second))
	}
	for _, line := range []string{"frame= 60", "out_time_ms=2000000", "speed=N/A", "progress=end"} {
		o.progressAt(line, observedStart.Add(2*time.Second))
	}
	s := o.snapshot()
	if !s.ProgressEnd || s.ProgressFrames != 60 || s.LastProgressTimeSeconds == nil || *s.LastProgressTimeSeconds != 2 || len(s.ProgressSamples) != 2 || s.ParseErrors != 0 {
		t.Fatal(s)
	}
	if *s.ProgressSamples[0].OutTimeUS != 1500000 || *s.ProgressSamples[0].Speed != 1.25 || s.ProgressSamples[0].AfterMS != 1000 || s.ProgressSamples[1].Speed != nil {
		t.Fatal(s.ProgressSamples)
	}
}

func TestMalformedNumbersDoNotCreateFrameEvidence(t *testing.T) {
	for _, bad := range []string{"NaN", "+Inf", "-Inf", "1e9999", "missing"} {
		t.Run(bad, func(t *testing.T) {
			o := newObserver(observedStart)
			o.stderr(videoLine(0, bad, "0.04"))
			o.stderr(videoLine(0, "0", bad))
			o.stderr(audioLine(0, bad, 1024, 48000))
			o.progress("speed=" + bad + "x")
			o.progress("out_time_us=" + bad)
			s := o.snapshot()
			if s.VideoFrames != 0 || s.AudioFrames != 0 || s.ParseErrors != 5 || s.DecodeError {
				t.Fatal(s)
			}
			if _, err := json.Marshal(s); err != nil {
				t.Fatal("nonfinite number reached JSON", err)
			}
		})
	}
	o := newObserver(observedStart)
	o.stderr(audioLine(0, "0", 1024, 0))
	o.stderr(audioLine(0, "0", -1, 48000))
	o.stderr(strings.Replace(videoLine(0, "0", "0.04"), "s:1280x720", "s:0x720", 1))
	o.progress("frame=-1")
	if s := o.snapshot(); s.ParseErrors != 4 || s.VideoFrames != 0 || s.AudioFrames != 0 {
		t.Fatal(s)
	}
}

func TestAudioSamplesUseEachFrameRateAndGuardOverflow(t *testing.T) {
	o := newObserver(observedStart)
	o.stderr(audioLine(0, "0", 480, 48000))
	o.stderr(audioLine(1, "0.01", 441, 44100))
	s := o.snapshot()
	if s.AudioSamples != 921 || math.Abs(s.AudioDurationSeconds-.02) > 1e-12 || len(s.AudioRates) != 2 || s.AudioRates[0] != 44100 {
		t.Fatal(s)
	}
	o.stats.AudioSamples = math.MaxInt64
	o.stderr(audioLine(2, "0.02", 1, 48000))
	if s = o.snapshot(); s.ParseErrors != 1 || s.AudioFrames != 2 || s.AudioSamples != math.MaxInt64 {
		t.Fatal(s)
	}
}

func TestFinitePTSValuesCannotOverflowDerivedSpan(t *testing.T) {
	o := newObserver(observedStart)
	o.stderr(videoLine(0, "-1e308", ""))
	o.stderr(videoLine(1, "1e308", ""))
	s := o.snapshot()
	if s.VideoFrames != 1 || s.ParseErrors != 1 || math.IsInf(s.VideoPTSSpanSeconds, 0) {
		t.Fatal(s)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal("derived nonfinite number reached JSON", err)
	}
}

func TestDiagnosticsClassifySpecificErrorsAndStayBounded(t *testing.T) {
	o := newObserver(observedStart)
	o.stderr("ffmpeg options: -max_error_rate 0 -xerror")
	o.stderr("[warning] deprecated pixel format used")
	o.stderr("[h264 @ 0x1234] error while decoding MB 12 4, bytestream -5")
	o.stderr("Conversion failed!")
	for i := 0; i < 30; i++ {
		o.stderr(fmt.Sprintf("diagnostic %d %s", i, strings.Repeat("ü", 1000)))
	}
	s := o.snapshot()
	if !s.DecodeError || s.Errors != 2 || s.Warnings != 1 || len(s.ErrorClasses) != 2 || len(s.Diagnostics) != 20 {
		t.Fatal(s)
	}
	for _, d := range s.Diagnostics {
		if len(d.Line) > 1024 || !utf8.ValidString(d.Line) {
			t.Fatal("unbounded or invalid UTF8 diagnostic")
		}
	}
	if !strings.HasPrefix(s.Diagnostics[0].Line, "diagnostic 10 ") {
		t.Fatal("diagnostics did not retain last 20")
	}
}

func TestLevelPrefixedLogsRetainMetadataAndFrameEvidence(t *testing.T) {
	o := newObserver(observedStart)
	for _, line := range []string{
		"[info] Input #0, mpegts, from 'pipe:0':",
		"[info]   Stream #0:0[0x100]: Video: h264 (Main), yuv420p, 1280x720",
		"[info]   Stream #0:1[0x101](eng): Audio: aac (LC), 48000 Hz, stereo, fltp",
		"[info] Stream mapping:",
		"[info] Output #0, null, to '/dev/null':",
		"[info]   Stream #0:0: Video: wrapped_avframe, yuv420p, 1280x720",
		"[info]   Stream #0:1: Audio: pcm_s16le, 48000 Hz, stereo",
		strings.Replace(videoLine(0, "0", "0.04"), "] n:", "] [info] n:", 1),
		"[info] " + audioLine(0, "0", 1024, 48000),
	} {
		o.stderrAt(line, observedStart)
	}
	s := o.snapshot()
	if s.InputVideoCodec != "h264" || s.InputAudioCodec != "aac" || s.VideoFrames != 1 || s.AudioFrames != 1 || s.ParseErrors != 0 || s.DecodeError {
		t.Fatal(s)
	}
}

func TestExplicitSeverityCountsUnknownDiagnostics(t *testing.T) {
	o := newObserver(observedStart)
	for _, line := range []string{
		"[info] option -max_error_rate=0 is enabled",
		"[info] literal message body containing [error] text",
		"[h264 @ 0x1234] [warning] unrecognized warning wording",
		"[error] unrecognized error wording",
		"[h264 @ 0x1234] [fatal] unrecognized fatal wording",
		"[panic] unrecognized panic wording",
		"[h264 @ 0x1234] [error] error while decoding MB 12 4",
	} {
		o.stderr(line)
	}
	s := o.snapshot()
	if s.Warnings != 1 || s.Errors != 4 || !s.DecodeError || s.ParseErrors != 0 || len(s.ErrorClasses) != 2 {
		t.Fatal(s)
	}
	if s.Diagnostics[0].Class != "info" || s.Diagnostics[1].Class != "info" || s.Diagnostics[2].Class != "warning" {
		t.Fatal(s.Diagnostics)
	}
}

func TestFrameIndexAnomaliesDoNotInventMissingFrames(t *testing.T) {
	for _, indexes := range [][]int{{0, 2, 3}, {0, 0, 1}, {0, 1, 0}, {4, 5, 6}} {
		t.Run(fmt.Sprint(indexes), func(t *testing.T) {
			o := newObserver(observedStart)
			for _, index := range indexes {
				o.stderr(videoLine(index, fmt.Sprint(float64(index)/25), "0.04"))
				o.stderr(audioLine(index, fmt.Sprint(float64(index)/100), 480, 48000))
			}
			s := o.snapshot()
			if s.VideoFrames != len(indexes) || s.AudioFrames != len(indexes) || s.AudioSamples != int64(len(indexes)*480) || s.ParseErrors != 2 || s.VideoSequenceErrors != 1 || s.AudioSequenceErrors != 1 || s.DecodeError {
				t.Fatal(s)
			}
		})
	}
}

func TestAudioCoreSurvivesObservedVideoColorLogInterleaving(t *testing.T) {
	// These two short metadata-only fragments preserve the real interleaving:
	// the audio av_log core has no component prefix and no preceding whitespace.
	for _, fixture := range []struct {
		index int
		line  string
	}{
		{3602, "[Parsed_showinfo_0 @ 0x12ae18dd0] [info] color_range:tvn:3602 pts:3691568 pts_time:76.907667 fmt:fltp channels:2 chlayout:stereo rate:48000 nb_samples:1024 checksum:3187ABA8 plane_checksums: [ 651DCE06 AB2CDD93 ]"},
		{6937, "[Parsed_showinfo_0 @ 0x12ae18dd0] [info] color_range:tvn:6937 pts:7109728 pts_time:148.119333 fmt:fltp channels:2 chlayout:stereo rate:48000 nb_samples:1024 checksum:41CA3517 plane_checksums: [ 090690F1 A0D4A417 ]"},
	} {
		o := newObserver(observedStart)
		// Supply only the sequence checkpoint, not invented prior frame counts.
		o.audioSequence = frameSequence{seen: true, last: fixture.index - 1}
		o.stderrAt(fixture.line, observedStart)
		s := o.snapshot()
		if s.VideoFrames != 0 || s.AudioFrames != 1 || s.AudioSamples != 1024 || s.ParseErrors != 0 || s.AudioSequenceErrors != 0 || s.DecodeError {
			t.Fatal(s)
		}
	}
}

func TestMultipleCompleteCoresOnOnePhysicalLineIgnorePrefixes(t *testing.T) {
	o := newObserver(observedStart)
	video := strings.SplitN(videoLine(0, "0", "0.04"), "] ", 2)[1]
	audio := strings.SplitN(audioLine(0, "0", 1024, 48000), "] ", 2)[1]
	// Neither core needs a component prefix. The following physical line also
	// has an audio prefix in front of a video core, as can happen during logging.
	o.stderrAt(video+audio, observedStart)
	o.stderrAt("[Parsed_ashowinfo_0 @ 0xabcd] [info] "+strings.SplitN(videoLine(1, "0.04", "0.04"), "] ", 2)[1]+" "+audioLine(1, "0.021333", 1024, 48000), observedStart.Add(time.Second))
	s := o.snapshot()
	if s.VideoFrames != 2 || s.AudioFrames != 2 || s.AudioSamples != 2048 || s.ParseErrors != 0 || s.Width != 1280 || s.PixelFormat != "yuv420p" {
		t.Fatal(s)
	}
}

func TestInterruptedCoreCannotBeReassembledFromOtherLogFragments(t *testing.T) {
	for _, line := range []string{
		"[Parsed_showinfo_0 @ 0x1234] n:0 pts:0 pts_time:0 [other @ 0x123] [info] injected fmt:yuv420p cl:left sar:1/1 s:1280x720 i:P iskey:1 type:I",
		"[Parsed_ashowinfo_0 @ 0xabcd] n:0 pts:0 pts_time:0 fmt:fltp channels:2 chlayout:stereo [other @ 0x123] rate:48000 nb_samples:1024",
		"[Parsed_showinfo_0 @ 0x1234] n:0 pts:0 fmt:yuv420p cl:left sar:1/1 s:1280x720 i:P iskey:1 type:I",
		"[Parsed_showinfo_0 @ 0x1234] n:0 pts_time:0 fmt:yuv420p cl:left sar:1/1 s:1280x720 i:P iskey:1 type:I",
	} {
		o := newObserver(observedStart)
		o.stderr(line)
		s := o.snapshot()
		if s.ParseErrors != 1 || s.VideoFrames != 0 || s.AudioFrames != 0 || s.AudioSamples != 0 {
			t.Fatal(s)
		}
	}
	o := newObserver(observedStart)
	o.stderr("n:0 pts:0 pts_time:0 fmt:yuv420p " + audioLine(0, "0", 1024, 48000) + " sar:1/1 s:1280x720 i:P iskey:1 type:I")
	if s := o.snapshot(); s.ParseErrors != 1 || s.VideoFrames != 0 || s.AudioFrames != 1 || s.AudioSamples != 1024 {
		t.Fatal("a complete audio core should survive beside an incomplete video core", s)
	}
}

// Only parse existing local evidence; never starts FFmpeg or makes a request.
func TestObserverOfflinePlaybackLoadLogs(t *testing.T) {
	configured := false
	for _, fixture := range []struct {
		env, name    string
		video, audio int
		samples      int64
	}{
		{"KICKPOP_PLAYBACK_OBSERVER_OLD_LOG", "looped_asset", 10792, 8431, 8633344},
		{"KICKPOP_PLAYBACK_OBSERVER_CONTINUOUS_LOG", "continuous_asset", 10800, 8437, 8639488},
	} {
		path := os.Getenv(fixture.env)
		if path == "" {
			continue
		}
		configured = true
		t.Run(fixture.name, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal("cannot open explicitly supplied local decoder log")
			}
			defer file.Close()
			o := newObserver(observedStart)
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 4096), 64<<10)
			for scanner.Scan() {
				o.stderrAt(scanner.Text(), observedStart)
			}
			if scanner.Err() != nil {
				t.Fatal("cannot read bounded local decoder log")
			}
			s := o.snapshot()
			if s.VideoFrames != fixture.video || s.AudioFrames != fixture.audio || s.AudioSamples != fixture.samples || s.ParseErrors != 0 || s.DecodeError {
				t.Fatalf("video=%d audio=%d samples=%d parse_errors=%d sequence_video=%d sequence_audio=%d decode_error=%v diagnostics=%+v", s.VideoFrames, s.AudioFrames, s.AudioSamples, s.ParseErrors, s.VideoSequenceErrors, s.AudioSequenceErrors, s.DecodeError, s.Diagnostics)
			}
			t.Logf("video=%d audio=%d samples=%d parse_errors=%d", s.VideoFrames, s.AudioFrames, s.AudioSamples, s.ParseErrors)
		})
	}
	if !configured {
		t.Skip("opt-in offline playback log regression")
	}
}

func TestProgressStorageBoundAndSnapshotIsolation(t *testing.T) {
	o := newObserver(observedStart)
	for i := 0; i < 600; i++ {
		o.progressAt(fmt.Sprintf("frame=%d", i), observedStart)
		o.progressAt(fmt.Sprintf("out_time_us=%d", i*1000), observedStart)
		o.progressAt("speed=1x", observedStart)
		o.progressAt("progress=continue", observedStart)
	}
	o.stderrAt(videoLine(0, "0", "0.04"), observedStart)
	s := o.snapshot()
	if len(s.ProgressSamples) != 512 || s.ProgressSamplesDropped != 88 || s.ProgressSamples[0].Frame != 88 {
		t.Fatal(s.ProgressSamplesDropped, len(s.ProgressSamples))
	}
	*s.FirstVideoPTS = 100
	*s.ProgressSamples[0].OutTimeUS = 123
	*s.ProgressSamples[0].Speed = 99
	s.ProgressSamples[0].Frame = -1
	fresh := o.snapshot()
	if *fresh.FirstVideoPTS != 0 || *fresh.ProgressSamples[0].OutTimeUS != 88000 || *fresh.ProgressSamples[0].Speed != 1 || fresh.ProgressSamples[0].Frame != 88 {
		t.Fatal("snapshot aliases observer state")
	}
}

func TestConcurrentReadersAndSnapshots(t *testing.T) {
	o := newObserver(time.Now())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			o.stderr(videoLine(i, fmt.Sprint(float64(i)/25), "0.04"))
			o.stderr(audioLine(i, fmt.Sprint(float64(i)/100), 480, 48000))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			o.progress(fmt.Sprintf("frame=%d", i))
			o.progress(fmt.Sprintf("out_time_us=%d", i*40000))
			o.progress("progress=continue")
		}
		o.progress("progress=end")
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = o.snapshot()
		}
	}()
	wg.Wait()
	s := o.snapshot()
	if s.VideoFrames != 100 || s.AudioFrames != 100 || !s.ProgressEnd || s.ParseErrors != 0 {
		t.Fatal(s)
	}
}
