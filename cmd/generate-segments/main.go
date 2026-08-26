// Command generate-segments backfills lossless gapless segment sets
// (fragmented ALAC and FLAC, with recorded fragment byte ranges) for track
// versions that predate the gapless playback feature. It also doubles as
// the repair tool for a version whose set previously failed: rerunning it
// picks up any version without a completed ALAC set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"bungleware/vault/internal/db"
	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/transcoding"
)

func main() {
	dataDir := flag.String("data-dir", "./data", "Path to data directory")
	dryRun := flag.Bool("dry-run", false, "Show what would be done without making changes")
	verbose := flag.Bool("verbose", false, "Show verbose output")
	flag.Parse()

	log.Println("=== Lossless Segment Backfill ===")
	log.Printf("Data directory: %s", *dataDir)
	log.Printf("Dry run: %v", *dryRun)
	log.Println()

	database, err := db.New(db.Config{
		DataDir:        *dataDir,
		DBFile:         "vault.db",
		MigrationsPath: "migrations",
	})
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	log.Println("Database connected successfully")

	ctx := context.Background()

	summary, err := runBackfill(ctx, database, *dryRun, *verbose)
	if err != nil {
		log.Fatalf("Backfill failed: %v", err)
	}

	log.Println()
	log.Printf("=== Results ===")
	log.Printf("  Processed: %d", summary.Processed)
	log.Printf("  Skipped (not lossless): %d", summary.SkippedNotLossless)
	log.Printf("  Skipped (missing source): %d", summary.SkippedMissingSource)
	log.Printf("  Failed: %d", summary.Failed)

	if *dryRun {
		log.Println()
		log.Println("DRY RUN - no changes were made. Run without -dry-run to build segments.")
	}
}

// backfillSummary counts what a run did, for the end-of-run report and for
// tests to assert on.
type backfillSummary struct {
	Processed            int
	SkippedNotLossless   int
	SkippedMissingSource int
	Failed               int
}

// runBackfill processes every lossless version missing a completed ALAC
// set. It is the whole command's logic, factored out of main so it can be
// exercised directly by tests against a throwaway database.
func runBackfill(ctx context.Context, database *db.DB, dryRun, verbose bool) (backfillSummary, error) {
	var summary backfillSummary

	rows, err := database.ListLosslessVersionsMissingSegments(ctx)
	if err != nil {
		return summary, fmt.Errorf("list versions missing segments: %w", err)
	}

	log.Printf("Found %d version(s) missing a completed lossless segment set", len(rows))
	log.Println()

	for _, row := range rows {
		if _, statErr := os.Stat(row.SourcePath); os.IsNotExist(statErr) {
			summary.SkippedMissingSource++
			if verbose {
				log.Printf("  - version %d: source not found at %s, skipping", row.VersionID, row.SourcePath)
			}
			continue
		}

		meta, metaErr := transcoding.ExtractMetadata(row.SourcePath)
		if metaErr != nil {
			// Unreadable source is treated the same as a missing one:
			// there is nothing to build segments from either way.
			summary.SkippedMissingSource++
			if verbose {
				log.Printf("  - version %d: failed to probe %s: %v, skipping", row.VersionID, row.SourcePath, metaErr)
			}
			continue
		}

		if !transcoding.IsLosslessCodec(meta.Codec) {
			summary.SkippedNotLossless++
			if verbose {
				log.Printf("  - version %d: source codec %q is not lossless, skipping", row.VersionID, meta.Codec)
			}
			continue
		}

		if dryRun {
			log.Printf("  DRY RUN - would build segments for version %d (%s, %s)", row.VersionID, meta.Codec, row.SourcePath)
			summary.Processed++
			continue
		}

		if verbose {
			log.Printf("  Processing version %d (%s): %s", row.VersionID, meta.Codec, row.SourcePath)
		}

		if err := backfillVersion(ctx, database, row.VersionID, row.SourcePath, meta.Codec); err != nil {
			log.Printf("  ✗ version %d: %v", row.VersionID, err)
			summary.Failed++
			continue
		}

		log.Printf("  ✓ version %d: segments built", row.VersionID)
		summary.Processed++
	}

	return summary, nil
}

// backfillVersion creates a pending segment-set row per codec, builds both
// codecs, and persists the results. On any failure it marks every set for
// this version failed rather than leaving a stale pending row behind, so a
// later run picks the version back up.
func backfillVersion(ctx context.Context, database *db.DB, versionID int64, sourcePath, sourceCodec string) error {
	versionDir := filepath.Dir(sourcePath)

	setIDs := make(map[string]int64, len(transcoding.SegmentCodecs))
	for _, codec := range transcoding.SegmentCodecs {
		outPath := filepath.Join(versionDir, "gapless-"+codec+".mp4")
		row, err := database.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
			VersionID: versionID,
			Codec:     codec,
			FilePath:  outPath,
		})
		if err != nil {
			return fmt.Errorf("create %s segment set: %w", codec, err)
		}
		setIDs[codec] = row.ID
	}

	sets, err := transcoding.BuildAllSegmentSets(sourcePath, versionDir, sourceCodec)
	if err != nil {
		failAllSegmentSets(ctx, database, setIDs)
		return fmt.Errorf("build segment sets: %w", err)
	}

	var persistErrs []error
	for _, set := range sets {
		id, ok := setIDs[set.Codec]
		if !ok {
			continue
		}
		if err := transcoding.PersistSegmentSet(database, id, set); err != nil {
			persistErrs = append(persistErrs, fmt.Errorf("persist %s: %w", set.Codec, err))
			if ferr := database.FailSegmentSet(ctx, id); ferr != nil {
				log.Printf("    failed to mark segment set %d failed: %v", id, ferr)
			}
		}
	}
	if len(persistErrs) > 0 {
		return errors.Join(persistErrs...)
	}

	return nil
}

// failAllSegmentSets marks every set in setIDs failed. Used when a shared
// build step (BuildAllSegmentSets) fails, since neither codec's set has
// output to persist.
func failAllSegmentSets(ctx context.Context, database *db.DB, setIDs map[string]int64) {
	for _, id := range setIDs {
		if err := database.FailSegmentSet(ctx, id); err != nil {
			log.Printf("    failed to mark segment set %d failed: %v", id, err)
		}
	}
}
