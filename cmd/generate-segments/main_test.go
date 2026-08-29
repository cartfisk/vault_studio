package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"bungleware/vault/internal/db"
	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/testutil"
	"bungleware/vault/internal/transcoding"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// makeSourceWAV writes seconds of 440Hz tone as 44.1kHz stereo pcm_s16le,
// matching the fixture used by internal/transcoding's own tests.
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

// seedSourceVersion creates a version whose "source" track_files row points
// at a real lossless WAV on disk, the shape ListLosslessVersionsMissingSegments
// expects.
func seedSourceVersion(t *testing.T, database *db.DB, seconds int) int64 {
	t.Helper()

	versionID := testutil.SeedVersion(t, database)
	wavPath := makeSourceWAV(t, t.TempDir(), seconds)

	if _, err := database.Exec(
		`INSERT INTO track_files (version_id, quality, file_path, file_size, format) VALUES (?, 'source', ?, 0, 'wav')`,
		versionID, wavPath,
	); err != nil {
		t.Fatalf("insert source track file error = %v", err)
	}

	return versionID
}

// TestRunBackfillIsIdempotent proves that running the backfill twice over
// the same seeded data only ever builds segments once: the second run must
// process zero versions and must not create duplicate track_segment_sets
// rows for a (version_id, codec) pair.
func TestRunBackfillIsIdempotent(t *testing.T) {
	requireFFmpeg(t)

	database := testutil.NewDB(t)
	versionID := seedSourceVersion(t, database, 25)
	ctx := context.Background()

	first, err := runBackfill(ctx, database, false, true)
	if err != nil {
		t.Fatalf("runBackfill() first run error = %v", err)
	}
	if first.Processed != 1 {
		t.Fatalf("first run Processed = %d, want 1 (%+v)", first.Processed, first)
	}
	if first.Failed != 0 {
		t.Fatalf("first run Failed = %d, want 0 (%+v)", first.Failed, first)
	}

	// Confirm the ALAC set actually completed - if it didn't,
	// ListLosslessVersionsMissingSegments would still return this version
	// and the "zero on second run" assertion below would be meaningless.
	completed, err := database.GetCompletedSegmentSet(ctx, sqlc.GetCompletedSegmentSetParams{
		VersionID: versionID,
		Codec:     "alac",
	})
	if err != nil {
		t.Fatalf("GetCompletedSegmentSet() error = %v, want a completed ALAC set after first run", err)
	}
	if completed.Status != "completed" {
		t.Fatalf("ALAC set status = %q, want completed", completed.Status)
	}

	second, err := runBackfill(ctx, database, false, true)
	if err != nil {
		t.Fatalf("runBackfill() second run error = %v", err)
	}
	if second.Processed != 0 {
		t.Errorf("second run Processed = %d, want 0", second.Processed)
	}
	if second.Failed != 0 {
		t.Errorf("second run Failed = %d, want 0 (%+v)", second.Failed, second)
	}

	// No duplicate (version_id, codec) rows in track_segment_sets.
	rows, err := database.Query(
		`SELECT codec, COUNT(*) FROM track_segment_sets WHERE version_id = ? GROUP BY codec HAVING COUNT(*) > 1`,
		versionID,
	)
	if err != nil {
		t.Fatalf("duplicate-check query error = %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var codec string
		var count int
		if err := rows.Scan(&codec, &count); err != nil {
			t.Fatalf("scan duplicate-check row error = %v", err)
		}
		t.Errorf("codec %q has %d track_segment_sets rows for version %d, want 1", codec, count, versionID)
	}
}

// TestBackfillDoesNotDisturbCompletedSetWhenOtherCodecFails is the
// regression test for the bug this branch fixes: backfillVersion used to
// call CreateSegmentSet (an upsert that resets status to pending and zeros
// measurements) for every codec before building anything. On a version
// whose ALAC set was already completed and whose FLAC set had failed --
// exactly the case the codec-agnostic query was changed to surface -- that
// reset the completed ALAC row to pending immediately, then
// BuildAllSegmentSets rebuilt ALAC and renamed it over the good file before
// FLAC failed again (its failure being persistent, which is why it failed
// the first time), and the cleanup path deleted every file it had built,
// including the just-renamed ALAC. A single backfill run over the one case
// it exists to repair turned a one-browser outage into a two-browser
// outage and deleted a good file from disk.
//
// This asserts the fixed behavior: after a backfill run whose FLAC build
// fails persistently, the pre-existing completed ALAC row and file are
// untouched -- same status, same measurements, identical bytes on disk.
func TestBackfillDoesNotDisturbCompletedSetWhenOtherCodecFails(t *testing.T) {
	requireFFmpeg(t)

	database := testutil.NewDB(t)
	ctx := context.Background()

	versionID := seedSourceVersion(t, database, 12)

	rows, err := database.ListLosslessVersionsMissingSegments(ctx)
	if err != nil {
		t.Fatalf("ListLosslessVersionsMissingSegments() error = %v", err)
	}
	var sourcePath string
	for _, row := range rows {
		if row.VersionID == versionID {
			sourcePath = row.SourcePath
		}
	}
	if sourcePath == "" {
		t.Fatalf("seeded version %d not found in ListLosslessVersionsMissingSegments()", versionID)
	}
	versionDir := filepath.Dir(sourcePath)

	// Build and complete a real ALAC set the ordinary way -- this is the
	// "already completed and serving" set that must survive.
	alacPath := filepath.Join(versionDir, "gapless-alac.mp4")
	alacSet, err := transcoding.BuildSegmentSet(sourcePath, alacPath, "alac", "pcm_s16le")
	if err != nil {
		t.Fatalf("BuildSegmentSet(alac) error = %v", err)
	}
	alacRow, err := database.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
		VersionID: versionID,
		Codec:     "alac",
		FilePath:  alacPath,
	})
	if err != nil {
		t.Fatalf("CreateSegmentSet(alac) error = %v", err)
	}
	if err := transcoding.PersistSegmentSet(database, alacRow.ID, alacSet); err != nil {
		t.Fatalf("PersistSegmentSet(alac) error = %v", err)
	}

	// Seed a previously-failed FLAC row, matching the case the
	// codec-agnostic backfill query was changed to surface.
	flacPath := filepath.Join(versionDir, "gapless-flac.mp4")
	flacRow, err := database.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
		VersionID: versionID,
		Codec:     "flac",
		FilePath:  flacPath,
	})
	if err != nil {
		t.Fatalf("CreateSegmentSet(flac) error = %v", err)
	}
	if err := database.FailSegmentSet(ctx, flacRow.ID); err != nil {
		t.Fatalf("FailSegmentSet(flac) error = %v", err)
	}

	// Force a deterministic, persistent FLAC build failure: pre-create the
	// flac output path as a directory, so ffmpeg refuses to write there
	// every time, the same way TestBuildAllSegmentSetsCleansUpPreviouslyBuiltFiles
	// forces it in internal/transcoding.
	if err := os.Mkdir(flacPath, 0755); err != nil {
		t.Fatalf("pre-create flac output path as a directory: %v", err)
	}

	wantBytes, err := os.ReadFile(alacPath)
	if err != nil {
		t.Fatalf("read alac file before backfill: %v", err)
	}

	summary, err := runBackfill(ctx, database, false, true)
	if err != nil {
		t.Fatalf("runBackfill() error = %v", err)
	}
	if summary.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (%+v)", summary.Failed, summary)
	}

	gotAlac, err := database.GetCompletedSegmentSet(ctx, sqlc.GetCompletedSegmentSetParams{
		VersionID: versionID,
		Codec:     "alac",
	})
	if err != nil {
		t.Fatalf("GetCompletedSegmentSet(alac) error = %v, want the ALAC set to still be completed after a persistent FLAC failure", err)
	}
	if gotAlac.SampleCount != alacSet.SampleCount {
		t.Errorf("ALAC SampleCount after backfill = %d, want unchanged %d", gotAlac.SampleCount, alacSet.SampleCount)
	}
	if gotAlac.FileSize != alacSet.FileSize {
		t.Errorf("ALAC FileSize after backfill = %d, want unchanged %d", gotAlac.FileSize, alacSet.FileSize)
	}

	gotBytes, err := os.ReadFile(alacPath)
	if err != nil {
		t.Fatalf("read alac file after backfill: %v", err)
	}
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Fatalf("ALAC file at %s changed after a backfill run whose FLAC build failed", alacPath)
	}
}
