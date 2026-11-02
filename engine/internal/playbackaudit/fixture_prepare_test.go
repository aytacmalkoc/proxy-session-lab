package playbackaudit

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/aytacmalkoc/proxy-session-lab/engine/internal/mediaprobe"
)

// TestCapturePlaybackFixture is an explicit one-clip capture for local load
// measurements. Normal tests never fetch media or write this fixture.
func TestCapturePlaybackFixture(t *testing.T) {
	if os.Getenv("KICKPOP_CAPTURE_PLAYBACK_FIXTURE") != "1" {
		t.Skip("opt-in one-clip capture")
	}
	path := os.Getenv("KICKPOP_CAPTURE_PLAYBACK_OUTPUT")
	if path == "" {
		t.Fatal("output path required")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	r, streamErr := mediaprobe.StreamVOD(ctx, mediaprobe.StreamConfig{
		URL:    "https://clips.kick.com/clips/76/clip_01M58BANG14VB6B1TRN110R0JN/playlist.m3u8",
		Client: "chrome_152", Timeout: 10 * time.Second,
	}, f)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := os.OpenFile(path+".json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(report)
	enc.SetIndent("", "  ")
	err = enc.Encode(r)
	report.Close()
	if err != nil {
		t.Fatal(err)
	}
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	if r.StopReason != "complete" || r.CompletedSegments != 8 || r.MediaDurationSeconds != 16 {
		t.Fatal("unexpected source clip contract")
	}
	t.Logf("captured %d full segments, %d bytes, %.3fs for local playback measurements", r.CompletedSegments, r.BytesWritten, r.MediaDurationSeconds)
}
