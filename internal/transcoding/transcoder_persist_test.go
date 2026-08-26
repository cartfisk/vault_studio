package transcoding

import (
	"context"
	"testing"

	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/testutil"
)

// TestPersistSegmentSetIsAtomic proves that a failure partway through
// persisting fragments leaves no partial fragment rows behind. A set marked
// failed must not still have fragments a byte-range handler could serve.
func TestPersistSegmentSetIsAtomic(t *testing.T) {
	database := testutil.NewDB(t)
	versionID := testutil.SeedVersion(t, database)
	ctx := context.Background()

	row, err := database.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
		VersionID: versionID,
		Codec:     "alac",
		FilePath:  "/tmp/whatever-alac.mp4",
	})
	if err != nil {
		t.Fatalf("CreateSegmentSet() error = %v", err)
	}

	tr := NewTranscoder(database, 0)

	set := &SegmentSet{
		Codec:       "alac",
		FilePath:    row.FilePath,
		FileSize:    1000,
		SampleRate:  44100,
		Channels:    2,
		SampleCount: 44100,
		Layout: FragmentLayout{
			InitByteEnd: 100,
			Fragments: []Fragment{
				{Start: 101, End: 200},
				{Start: 201, End: 300},
				// Inverted range: violates the byte_end >= byte_start
				// CHECK constraint, so this insert fails partway through.
				{Start: 500, End: 400},
				{Start: 401, End: 500},
			},
		},
	}

	if err := tr.persistSegmentSet(ctx, row.ID, set); err == nil {
		t.Fatal("persistSegmentSet() error = nil, want error from inverted fragment range")
	}

	fragments, err := database.ListSegmentFragments(ctx, row.ID)
	if err != nil {
		t.Fatalf("ListSegmentFragments() error = %v", err)
	}
	if len(fragments) != 0 {
		t.Errorf("fragment rows after failed persist = %d, want 0 (found %+v)", len(fragments), fragments)
	}
}
