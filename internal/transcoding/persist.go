package transcoding

import (
	"context"
	"fmt"

	"bungleware/vault/internal/db"
	sqlc "bungleware/vault/internal/db/sqlc"
)

// PersistSegmentSet writes a built SegmentSet's fragment rows and marks the
// set completed, in a single transaction.
//
// The transaction exists because a partial fragment write leaves byte
// ranges that only partially describe a file, which produces corrupt audio
// in a browser far from the cause. Both the async transcoder job and the
// generate-segments backfill command call this rather than each carrying
// its own copy of the same transactional logic.
func PersistSegmentSet(database *db.DB, setID int64, set *SegmentSet) error {
	ctx := context.Background()

	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	qtx := database.Queries.WithTx(tx)

	if err := qtx.DeleteSegmentFragments(ctx, setID); err != nil {
		return fmt.Errorf("clear fragments: %w", err)
	}

	for i, frag := range set.Layout.Fragments {
		err := qtx.CreateSegmentFragment(ctx, sqlc.CreateSegmentFragmentParams{
			SetID:     setID,
			Idx:       int64(i),
			ByteStart: frag.Start,
			ByteEnd:   frag.End,
		})
		if err != nil {
			return fmt.Errorf("insert fragment %d: %w", i, err)
		}
	}

	if err := qtx.CompleteSegmentSet(ctx, sqlc.CompleteSegmentSetParams{
		FileSize:    set.FileSize,
		SampleRate:  int64(set.SampleRate),
		SampleCount: set.SampleCount,
		Channels:    int64(set.Channels),
		InitByteEnd: set.Layout.InitByteEnd,
		ID:          setID,
	}); err != nil {
		return fmt.Errorf("complete segment set: %w", err)
	}

	return tx.Commit()
}
