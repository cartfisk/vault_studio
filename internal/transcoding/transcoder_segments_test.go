package transcoding_test

import (
	"context"
	"testing"

	"bungleware/vault/internal/testutil"
	"bungleware/vault/internal/transcoding"
)

func TestTranscodeVersionSkipsSegmentsForLossySource(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)

	tr := transcoding.NewTranscoder(database, 0) // no workers; inspect the rows only

	err := tr.TranscodeVersion(context.Background(), transcoding.TranscodeVersionInput{
		VersionID:      versionID,
		SourceFilePath: "/tmp/whatever.mp3",
		TrackPublicID:  "abc",
		UserID:         1,
		SourceCodec:    "mp3",
	})
	if err != nil {
		t.Fatalf("TranscodeVersion() error = %v", err)
	}

	var count int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM track_segment_sets WHERE version_id = ?`, versionID,
	).Scan(&count); err != nil {
		t.Fatalf("count error = %v", err)
	}
	if count != 0 {
		t.Errorf("segment sets for a lossy source = %d, want 0", count)
	}
}

func TestTranscodeVersionCreatesPendingSetsForLosslessSource(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)

	tr := transcoding.NewTranscoder(database, 0)

	err := tr.TranscodeVersion(context.Background(), transcoding.TranscodeVersionInput{
		VersionID:      versionID,
		SourceFilePath: "/tmp/whatever.wav",
		TrackPublicID:  "abc",
		UserID:         1,
		SourceCodec:    "pcm_s16le",
	})
	if err != nil {
		t.Fatalf("TranscodeVersion() error = %v", err)
	}

	rows, err := database.Query(
		`SELECT codec, status FROM track_segment_sets WHERE version_id = ? ORDER BY codec`,
		versionID,
	)
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var codec, status string
		if err := rows.Scan(&codec, &status); err != nil {
			t.Fatalf("scan error = %v", err)
		}
		got = append(got, codec+":"+status)
	}

	want := []string{"alac:pending", "flac:pending"}
	if len(got) != len(want) {
		t.Fatalf("sets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sets[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
