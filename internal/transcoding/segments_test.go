package transcoding

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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
// off-limits. See TestBuildAllSegmentSetsRemovesStagedFilesOnBuildFailure
// below for a genuine, non-vacuous exercise of the cleanup-on-failure
// behavior.
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

// TestBuildSegmentSetIsAtomicToConcurrentReaders proves BuildSegmentSet
// never truncates outputPath in place while a reader (e.g.
// http.ServeContent serving a previously completed set, or a concurrent
// backfill run) has it open. It pre-creates outputPath with known content
// and opens a descriptor on it before calling BuildSegmentSet -- if the
// implementation writes straight to outputPath (ffmpeg -y opens with
// O_TRUNC), that truncates the file underneath the already-open
// descriptor, corrupting whatever the fictitious reader is mid-serving.
// An atomic write-to-tmp-then-rename instead leaves the pre-opened
// descriptor pointing at the old, now-unlinked inode: a read through it
// after the call returns must still see the exact original bytes.
func TestBuildSegmentSetIsAtomicToConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 12)
	out := filepath.Join(dir, "gapless-alac.mp4")

	oldContent := []byte("old-completed-segment-set-bytes-a-reader-is-mid-serving")
	if err := os.WriteFile(out, oldContent, 0o644); err != nil {
		t.Fatalf("write pre-existing output: %v", err)
	}

	reader, err := os.Open(out)
	if err != nil {
		t.Fatalf("open pre-existing output: %v", err)
	}
	defer reader.Close()

	set, err := BuildSegmentSet(src, out, "alac", "pcm_s16le")
	if err != nil {
		t.Fatalf("BuildSegmentSet() error = %v", err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read from pre-opened descriptor: %v", err)
	}
	if !bytes.Equal(got, oldContent) {
		t.Fatalf("bytes changed underneath a pre-opened reader: want %q, got %q (%d bytes)", oldContent, got, len(got))
	}

	if set.FilePath != out {
		t.Errorf("SegmentSet.FilePath = %q, want final path %q, not a tmp path", set.FilePath, out)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat final output: %v", err)
	}
	if info.Size() == int64(len(oldContent)) {
		t.Fatalf("output at %s was not replaced by the new build", out)
	}

	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp file left behind at %s: stat err = %v", out+".tmp", err)
	}
}

// TestBuildAllSegmentSetsRemovesStagedFilesOnBuildFailure forces a
// genuine ffmpeg BUILD-stage failure (not a preflight or commit-stage
// one) by pointing sourcePath at a file ffmpeg cannot decode as audio, and
// asserts the version directory ends up holding nothing but that source:
// no staged file survives, and no final file for either codec appears.
//
// This test used to force its failure by pre-creating a directory at the
// flac output path, back when BuildSegmentSet wrote straight to that
// path. Since BuildAllSegmentSets started building into unique staging
// files first, that trick no longer touches the build at all -- ffmpeg
// never goes near the blocked path -- it only trips
// preflightCommitDestinations, which
// TestBuildAllSegmentSetsPreflightsAllDestinationsBeforeCommitting below
// already covers. Repurposed here to cover the build-stage path instead,
// which is the central new mechanism (unique per-codec staging, cleaned
// up on failure) this file's atomic-commit rework added and which was
// left with no real coverage.
//
// The failure is forced on the FIRST codec (alac) rather than only the
// second (flac, after alac has already succeeded). Making it fail only on
// the second codec deterministically would mean swapping the source out
// from under BuildAllSegmentSets between its two BuildSegmentSet calls --
// a timing dependency for no real gain, since the staged-file cleanup
// this test exists to cover doesn't care which codec failed, only that a
// build failure happened after some staging files already existed.
func TestBuildAllSegmentSetsRemovesStagedFilesOnBuildFailure(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()

	src := filepath.Join(dir, "source.wav")
	if err := os.WriteFile(src, []byte("not actually audio"), 0o644); err != nil {
		t.Fatalf("write bogus source: %v", err)
	}

	if _, err := BuildAllSegmentSets(src, dir, "pcm_s16le"); err == nil {
		t.Fatal("BuildAllSegmentSets() error = nil for a non-audio source, want error")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read version dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "source.wav" {
			continue
		}
		t.Errorf("unexpected leftover entry %q in version dir after a build failure", e.Name())
	}
}

// TestBuildAllSegmentSetsPreflightsAllDestinationsBeforeCommitting proves
// the commit step checks every codec's final destination before renaming
// any of them, rather than discovering a blocked destination partway
// through the commit loop and leaving a partial commit behind. It forces
// a deterministic failure by pre-creating a directory at the flac output
// path, so preflightCommitDestinations (not the build, which now targets
// a unique staging file first) refuses that destination. Since ALAC's
// build succeeds first, an implementation that renamed as it goes (rather
// than checking every destination up front) would commit
// gapless-alac.mp4 before ever attempting FLAC's blocked rename.
// Preflighting both destinations first must catch the problem before that
// ALAC rename ever runs.
func TestBuildAllSegmentSetsPreflightsAllDestinationsBeforeCommitting(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 12)

	flacOut := filepath.Join(dir, "gapless-flac.mp4")
	if err := os.Mkdir(flacOut, 0755); err != nil {
		t.Fatalf("pre-create flac output path as a directory: %v", err)
	}
	alacOut := filepath.Join(dir, "gapless-alac.mp4")

	if _, err := BuildAllSegmentSets(src, dir, "pcm_s16le"); err == nil {
		t.Fatal("BuildAllSegmentSets() error = nil, want error from the blocked flac destination")
	}

	if _, statErr := os.Stat(alacOut); !os.IsNotExist(statErr) {
		t.Errorf("alac final file committed at %s despite the blocked flac destination: stat err = %v", alacOut, statErr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read version dir: %v", err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "source.wav", "gapless-flac.mp4":
			continue // pre-existing fixtures, not this call's output
		}
		t.Errorf("unexpected leftover entry %q in version dir after preflight failure", e.Name())
	}
}

// TestBuildAllSegmentSetsSweepsStaleStagingFiles proves BuildAllSegmentSets
// removes an orphaned staging file old enough to be from a killed run,
// while leaving a recent one -- which could belong to a build genuinely in
// progress -- untouched. It plants one staged-looking file per codec
// pattern with an old mtime (via os.Chtimes) and one with a fresh mtime,
// then runs a real build and checks which survived.
func TestBuildAllSegmentSetsSweepsStaleStagingFiles(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceWAV(t, dir, 6)

	oldStaging := filepath.Join(dir, "gapless-alac-oldorphan.mp4")
	if err := os.WriteFile(oldStaging, []byte("orphaned by a killed run"), 0o644); err != nil {
		t.Fatalf("write old staging file: %v", err)
	}
	old := time.Now().Add(-2 * staleStagingAge)
	if err := os.Chtimes(oldStaging, old, old); err != nil {
		t.Fatalf("Chtimes old staging file: %v", err)
	}

	recentStaging := filepath.Join(dir, "gapless-flac-inprogress.mp4")
	if err := os.WriteFile(recentStaging, []byte("a build genuinely in progress"), 0o644); err != nil {
		t.Fatalf("write recent staging file: %v", err)
	}
	// Leave recentStaging's mtime at "now" -- it must look live.

	if _, err := BuildAllSegmentSets(src, dir, "pcm_s16le"); err != nil {
		t.Fatalf("BuildAllSegmentSets() error = %v", err)
	}

	if _, statErr := os.Stat(oldStaging); !os.IsNotExist(statErr) {
		t.Errorf("stale staging file %s survived a build, want swept: stat err = %v", oldStaging, statErr)
	}
	if _, statErr := os.Stat(recentStaging); statErr != nil {
		t.Errorf("recent staging file %s was swept, want preserved: %v", recentStaging, statErr)
	}
}

// TestSweepStaleStagingFilesNeverMatchesAFinalPath verifies, rather than
// assumes, that the sweep's glob pattern cannot match a completed set's
// final path: "gapless-alac.mp4" has no hyphen after the codec, while the
// pattern requires one ("gapless-alac-*.mp4*"). A final file, even one
// old enough that an age-only check would remove it, must survive.
func TestSweepStaleStagingFilesNeverMatchesAFinalPath(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "gapless-alac.mp4")
	if err := os.WriteFile(finalPath, []byte("a real completed segment set"), 0o644); err != nil {
		t.Fatalf("write final file: %v", err)
	}
	old := time.Now().Add(-2 * staleStagingAge)
	if err := os.Chtimes(finalPath, old, old); err != nil {
		t.Fatalf("Chtimes final file: %v", err)
	}

	if err := sweepStaleFiles(filepath.Join(dir, "gapless-alac-*.mp4*"), staleStagingAge); err != nil {
		t.Fatalf("sweepStaleFiles() error = %v", err)
	}

	if _, statErr := os.Stat(finalPath); statErr != nil {
		t.Fatalf("final file %s was swept: %v", finalPath, statErr)
	}
}

// TestSegmentCodecsCountMatchesSQLLiteral is the cheapest real link between
// SegmentCodecs and the hardcoded 2 in
// internal/db/queries/segments.sql's ListLosslessVersionsMissingSegments.
// That query treats a version as "done" once its count of completed sets
// reaches 2; if SegmentCodecs ever changes length without updating the SQL,
// a version with N-1 of N codecs completed would read as done and silently
// stop being returned for repair.
func TestSegmentCodecsCountMatchesSQLLiteral(t *testing.T) {
	if len(SegmentCodecs) != 2 {
		t.Fatalf(
			"len(SegmentCodecs) = %d, want 2 -- update the hardcoded 2 in "+
				"internal/db/queries/segments.sql's ListLosslessVersionsMissingSegments "+
				"(and regenerate sqlc) to match, then update this test",
			len(SegmentCodecs),
		)
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
