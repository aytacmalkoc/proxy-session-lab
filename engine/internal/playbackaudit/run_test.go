package playbackaudit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/mediaprobe"
)

func TestLineWriterBoundedAndSplitRecords(t *testing.T) {
	var lines []string
	w := &lineWriter{line: func(line string) { lines = append(lines, line) }}
	for _, p := range []string{"fra", "me=1\r\nprog", "ress=end\nlast"} {
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	if strings.Join(lines, "|") != "frame=1|progress=end|last" {
		t.Fatal(lines)
	}
	if _, err := w.Write(bytes.Repeat([]byte{'x'}, (64<<10)+1)); err == nil {
		t.Fatal("unbounded line accepted")
	}
}

// Optional integration test with generated, local, non-user media. It never
// contacts the endpoint; the production network path has separate TLS fixtures.
func TestLocalFFmpegDecodeAndCancellation(t *testing.T) {
	decoder, fixture := os.Getenv("KICKPOP_FFMPEG"), os.Getenv("KICKPOP_DECODER_FIXTURE")
	if decoder == "" || fixture == "" {
		t.Skip("opt-in local decoder integration")
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{URL: "https://fixture.invalid/playlist.m3u8", Client: "stdlib", Decoder: decoder, Timeout: 15 * time.Second, RequestTimeout: time.Second}
	t.Run("decode", func(t *testing.T) {
		source := func(ctx context.Context, _ mediaprobe.StreamConfig, sink io.Writer) (mediaprobe.StreamReport, error) {
			n, err := sink.Write(data)
			return mediaprobe.StreamReport{StopReason: "complete", SegmentCount: 1, CompletedSegments: 1, DeclaredBytes: int64(len(data)), BytesDownloaded: int64(len(data)), BytesWritten: int64(n), MediaDurationSeconds: 2}, err
		}
		r, err := run(context.Background(), cfg, source)
		if err != nil {
			t.Fatalf("%v\nstats: %+v", err, r.Decoder.Stats)
		}
		if !r.Completed || r.Decoder.Stats.VideoFrames != 50 || r.Decoder.Stats.AudioSamples <= 0 || r.Decoder.ExitCode != 0 || r.ElapsedSeconds < 1.9 {
			t.Fatalf("invalid decode evidence: %+v", r)
		}
		t.Logf("frames=%d audio_samples=%d video_span=%f elapsed=%f", r.Decoder.Stats.VideoFrames, r.Decoder.Stats.AudioSamples, r.Decoder.Stats.VideoPTSSpanSeconds, r.ElapsedSeconds)
	})
	t.Run("duration_mismatch", func(t *testing.T) {
		source := func(ctx context.Context, _ mediaprobe.StreamConfig, sink io.Writer) (mediaprobe.StreamReport, error) {
			n, err := sink.Write(data)
			return mediaprobe.StreamReport{StopReason: "complete", SegmentCount: 1, CompletedSegments: 1, DeclaredBytes: int64(len(data)), BytesDownloaded: int64(len(data)), BytesWritten: int64(n), MediaDurationSeconds: 16}, err
		}
		r, err := run(context.Background(), cfg, source)
		if err == nil || r.Completed || r.StopReason != "duration_mismatch" || r.Decoder.ExitCode != 0 {
			t.Fatalf("short media was declared complete: %v %+v", err, r.Coverage)
		}
	})
	t.Run("cancel_blocked_writer", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := func(ctx context.Context, _ mediaprobe.StreamConfig, sink io.Writer) (mediaprobe.StreamReport, error) {
			timer := time.AfterFunc(50*time.Millisecond, cancel)
			defer timer.Stop()
			_, err := sink.Write(bytes.Repeat(data, 8))
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return mediaprobe.StreamReport{}, err
		}
		start := time.Now()
		r, err := run(ctx, cfg, source)
		if !errors.Is(err, context.Canceled) || r.Completed || r.Decoder.ExitCode != -1 || time.Since(start) > 3*time.Second {
			t.Fatalf("decoder cancellation failed: %v %+v", err, r)
		}
	})
}
