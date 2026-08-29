package db_test

import (
	"context"
	"testing"

	"bungleware/vault/internal/testutil"
)

func TestSegmentSetsUniquePerVersionAndCodec(t *testing.T) {
	database := testutil.NewDB(t)

	versionID := testutil.SeedVersion(t, database)

	insert := `INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`
	if _, err := database.Exec(insert, versionID, "alac", "/tmp/a.mp4"); err != nil {
		t.Fatalf("first insert error = %v", err)
	}
	if _, err := database.Exec(insert, versionID, "flac", "/tmp/f.mp4"); err != nil {
		t.Fatalf("second codec insert error = %v", err)
	}
	if _, err := database.Exec(insert, versionID, "alac", "/tmp/dup.mp4"); err == nil {
		t.Fatal("duplicate (version_id, codec) was accepted, want constraint violation")
	}
}

func TestSegmentSetRejectsUnknownCodec(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)

	_, err := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`,
		versionID, "aac", "/tmp/a.mp4",
	)
	if err == nil {
		t.Fatal("codec 'aac' was accepted, want CHECK violation")
	}
}

func TestFragmentsCascadeOnSetDelete(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)

	res, err := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`,
		versionID, "alac", "/tmp/a.mp4",
	)
	if err != nil {
		t.Fatalf("insert set error = %v", err)
	}
	setID, _ := res.LastInsertId()

	if _, err := database.Exec(
		`INSERT INTO track_segment_fragments (set_id, idx, byte_start, byte_end) VALUES (?, ?, ?, ?)`,
		setID, 0, 1024, 2047,
	); err != nil {
		t.Fatalf("insert fragment error = %v", err)
	}

	if _, err := database.Exec(`DELETE FROM track_segment_sets WHERE id = ?`, setID); err != nil {
		t.Fatalf("delete set error = %v", err)
	}

	var count int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM track_segment_fragments WHERE set_id = ?`, setID,
	).Scan(&count); err != nil {
		t.Fatalf("count error = %v", err)
	}
	if count != 0 {
		t.Fatalf("fragments after cascade = %d, want 0", count)
	}
}

// TestListLosslessVersionsMissingSegmentsCatchesFlacOnlyFailure proves that
// backfill picks up a version whose ALAC set completed but whose FLAC set
// failed. Before this query was made codec-agnostic, it keyed only on
// codec = 'alac', so this shape -- reachable whenever one codec's ffmpeg
// invocation fails and the other's succeeds -- was invisible to backfill
// forever: Safari would play gapless, Chrome and Firefox never would.
func TestListLosslessVersionsMissingSegmentsCatchesFlacOnlyFailure(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)

	if _, err := database.Exec(
		`INSERT INTO track_files (version_id, quality, file_path, file_size, format) VALUES (?, 'source', ?, 0, 'wav')`,
		versionID, "/tmp/source.wav",
	); err != nil {
		t.Fatalf("insert source track file error = %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path, status) VALUES (?, 'alac', '/tmp/gapless-alac.mp4', 'completed')`,
		versionID,
	); err != nil {
		t.Fatalf("insert completed alac set error = %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path, status) VALUES (?, 'flac', '/tmp/gapless-flac.mp4', 'failed')`,
		versionID,
	); err != nil {
		t.Fatalf("insert failed flac set error = %v", err)
	}

	rows, err := database.ListLosslessVersionsMissingSegments(context.Background())
	if err != nil {
		t.Fatalf("ListLosslessVersionsMissingSegments() error = %v", err)
	}

	var found bool
	for _, row := range rows {
		if row.VersionID == versionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("version %d (alac completed, flac failed) not returned; backfill would never repair it", versionID)
	}
}

func TestFragmentRejectsInvertedRange(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)

	res, _ := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`,
		versionID, "alac", "/tmp/a.mp4",
	)
	setID, _ := res.LastInsertId()

	_, err := database.Exec(
		`INSERT INTO track_segment_fragments (set_id, idx, byte_start, byte_end) VALUES (?, ?, ?, ?)`,
		setID, 0, 2048, 1024,
	)
	if err == nil {
		t.Fatal("byte_end < byte_start was accepted, want CHECK violation")
	}
}
