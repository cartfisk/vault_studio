package transcoding

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// makeSourceWAV writes seconds of 440Hz tone as 44.1kHz stereo pcm_s16le.
func makeSourceWAV(t *testing.T, dir string, seconds int) string {
	t.Helper()
	requireFFmpeg(t)

	path := filepath.Join(dir, "source.wav")
	cmd := exec.Command("ffmpeg", "-v", "error",
		"-f", "lavfi",
		"-i", fmt.Sprintf("sine=frequency=440:sample_rate=44100:duration=%d", seconds),
		"-ac", "2", "-c:a", "pcm_s16le", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture ffmpeg failed: %v: %s", err, out)
	}
	return path
}

func TestBuildSegmentSetALACSampleCount(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 25)

	set, err := BuildSegmentSet(src, filepath.Join(dir, "gapless-alac.mp4"), "alac", "pcm_s16le")
	if err != nil {
		t.Fatalf("BuildSegmentSet() error = %v", err)
	}

	if set.SampleRate != 44100 {
		t.Errorf("SampleRate = %d, want 44100", set.SampleRate)
	}
	if set.Channels != 2 {
		t.Errorf("Channels = %d, want 2", set.Channels)
	}
	if want := int64(25 * 44100); set.SampleCount != want {
		t.Errorf("SampleCount = %d, want %d", set.SampleCount, want)
	}
}

func TestBuildSegmentSetFragmentsALongTrack(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 25) // longer than one 10s fragment

	set, err := BuildSegmentSet(src, filepath.Join(dir, "gapless-alac.mp4"), "alac", "pcm_s16le")
	if err != nil {
		t.Fatalf("BuildSegmentSet() error = %v", err)
	}
	if len(set.Layout.Fragments) < 2 {
		t.Fatalf("Fragments = %d for a 25s track at a 10s fragment duration, want >= 2",
			len(set.Layout.Fragments))
	}
	if set.Layout.InitByteEnd <= 0 {
		t.Errorf("InitByteEnd = %d, want > 0", set.Layout.InitByteEnd)
	}
}

func TestBuildSegmentSetFLACStreamCopy(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 12)

	flacSrc := filepath.Join(dir, "source.flac")
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", src, "-c:a", "flac", "-y", flacSrc)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("flac fixture failed: %v: %s", err, out)
	}

	set, err := BuildSegmentSet(flacSrc, filepath.Join(dir, "gapless-flac.mp4"), "flac", "flac")
	if err != nil {
		t.Fatalf("BuildSegmentSet() error = %v", err)
	}
	if want := int64(12 * 44100); set.SampleCount != want {
		t.Errorf("SampleCount = %d, want %d", set.SampleCount, want)
	}
}

func TestBuildAllSegmentSetsAgreeOnSampleCount(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 25)

	sets, err := BuildAllSegmentSets(src, dir, "pcm_s16le")
	if err != nil {
		t.Fatalf("BuildAllSegmentSets() error = %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("sets = %d, want 2", len(sets))
	}
	if sets[0].SampleCount != sets[1].SampleCount {
		t.Errorf("%s SampleCount = %d, %s SampleCount = %d, want equal",
			sets[0].Codec, sets[0].SampleCount, sets[1].Codec, sets[1].SampleCount)
	}

	for _, s := range sets {
		if _, err := os.Stat(s.FilePath); err != nil {
			t.Errorf("%s output missing: %v", s.Codec, err)
		}
	}
}

func TestBuildSegmentSetAllowsSingleFragmentForShortTrack(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 4) // shorter than one 10s fragment

	set, err := BuildSegmentSet(src, filepath.Join(dir, "gapless-alac.mp4"), "alac", "pcm_s16le")
	if err != nil {
		t.Fatalf("BuildSegmentSet() error = %v, want a short track to be accepted", err)
	}
	if len(set.Layout.Fragments) < 1 {
		t.Fatalf("Fragments = %d, want >= 1", len(set.Layout.Fragments))
	}
}

func TestBuildSegmentSetRejectsUnknownCodec(t *testing.T) {
	dir := t.TempDir()
	if _, err := BuildSegmentSet("/nonexistent.wav", filepath.Join(dir, "x.mp4"), "aac", "pcm_s16le"); err == nil {
		t.Fatal("BuildSegmentSet() error = nil for codec 'aac', want error")
	}
}
