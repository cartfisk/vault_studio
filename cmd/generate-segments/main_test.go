package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"bungleware/vault/internal/db"
	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/testutil"
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
