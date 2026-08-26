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

// TestBuildSegmentSetLeavesNoFileOnFFmpegFailure covers the early-failure
// path: ffmpeg cannot open a nonexistent source, so it never writes
// outputPath in the first place (verified independently: ffmpeg exits before
// creating the output file when the input open fails). This does not
// exercise "remove an already-written file", since no write happens — that
// case would require a post-write failure (probe, scan, or the
// not-fragmented check), which could not be triggered deterministically
// without either breaking fragMovFlags or contriving a broken encoder, both
// off-limits. See TestBuildAllSegmentSetsCleansUpPreviouslyBuiltFiles below
// for a genuine, non-vacuous exercise of the cleanup-on-failure behavior.
func TestBuildSegmentSetLeavesNoFileOnFFmpegFailure(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "gapless-alac.mp4")

	if _, err := BuildSegmentSet(filepath.Join(dir, "nonexistent.wav"), out, "alac", "pcm_s16le"); err == nil {
		t.Fatal("BuildSegmentSet() error = nil for a nonexistent source, want error")
	}

	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Errorf("output file left behind at %s after ffmpeg failure: stat err = %v", out, statErr)
	}
}

// TestBuildAllSegmentSetsCleansUpPreviouslyBuiltFiles forces a genuine,
// deterministic failure on the second codec (flac) without touching the
// encoder: it pre-creates a directory at the flac output path, so ffmpeg
// refuses to open it ("Is a directory") after alac has already been built
// successfully. It then asserts BuildAllSegmentSets removed the alac file it
// had already written, rather than leaving a plausible-looking gapless-alac.mp4
// for a version whose flac set never materialized.
func TestBuildAllSegmentSetsCleansUpPreviouslyBuiltFiles(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 12)

	flacOut := filepath.Join(dir, "gapless-flac.mp4")
	if err := os.Mkdir(flacOut, 0755); err != nil {
		t.Fatalf("pre-create flac output path as a directory: %v", err)
	}

	alacOut := filepath.Join(dir, "gapless-alac.mp4")

	if _, err := BuildAllSegmentSets(src, dir, "pcm_s16le"); err == nil {
		t.Fatal("BuildAllSegmentSets() error = nil, want error from the blocked flac output path")
	}

	if _, statErr := os.Stat(alacOut); !os.IsNotExist(statErr) {
		t.Errorf("alac output left behind at %s after flac failure: stat err = %v", alacOut, statErr)
	}
}

func TestSampleCountFor(t *testing.T) {
	tests := []struct {
		name                       string
		durationTS, num, den, rate int64
		want                       int64
		wantErr                    bool
	}{
		{
			name:       "1/44100 time base at 44100Hz",
			durationTS: 1102500, num: 1, den: 44100, rate: 44100,
			want: 1102500,
		},
		{
			name:       "1/1000 millisecond time base divides evenly",
			durationTS: 25000, num: 1, den: 1000, rate: 44100,
			want: 25 * 44100,
		},
		{
			name:       "does not divide evenly",
			durationTS: 100, num: 1, den: 3, rate: 1,
			wantErr: true,
		},
		{
			name:       "zero result",
			durationTS: 0, num: 1, den: 44100, rate: 44100,
			wantErr: true,
		},
		{
			name:       "negative result",
			durationTS: -100, num: 1, den: 44100, rate: 44100,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sampleCountFor(tt.durationTS, tt.num, tt.den, tt.rate)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sampleCountFor() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("sampleCountFor() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("sampleCountFor() = %d, want %d", got, tt.want)
			}
		})
	}
}
