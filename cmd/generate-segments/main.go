// Command generate-segments backfills lossless gapless segment sets
// (fragmented ALAC and FLAC, with recorded fragment byte ranges) for track
// versions that predate the gapless playback feature. It also doubles as
// the repair tool for a version whose set previously failed: rerunning it
// picks up any version missing a completed set for any codec.
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

// runBackfill processes every lossless version missing a completed segment
// set for some codec. It is the whole command's logic, factored out of main
// so it can be exercised directly by tests against a throwaway database.
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

// backfillVersion builds both codecs first and only touches the database
// once the build has succeeded. A version reaches here precisely because
// some codec's set is missing or failed while another may already be
// completed and serving; creating rows before building would reset that
// completed row to pending, and BuildAllSegmentSets's atomic-rename
// wouldn't save it if the other codec then failed persistently, since a
// rebuilt copy would still land on top of it. Building first means a
// build failure never touches an existing row at all, and the query picks
// the version back up on the next run, which is correct for a repair
// tool. The tradeoff: a version with no rows yet whose build fails leaves
// no database record at all, so an operator reading the table directly
// cannot distinguish "never attempted" from "fails every time" without
// checking the command's own log output.
func backfillVersion(ctx context.Context, database *db.DB, versionID int64, sourcePath, sourceCodec string) error {
	versionDir := filepath.Dir(sourcePath)

	sets, err := transcoding.BuildAllSegmentSets(sourcePath, versionDir, sourceCodec)
	if err != nil {
		return fmt.Errorf("build segment sets: %w", err)
	}

	var persistErrs []error
	for _, set := range sets {
		row, err := database.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
			VersionID: versionID,
			Codec:     set.Codec,
			FilePath:  set.FilePath,
		})
		if err != nil {
			persistErrs = append(persistErrs, fmt.Errorf("create %s segment set: %w", set.Codec, err))
			continue
		}
		if err := transcoding.PersistSegmentSet(database, row.ID, set); err != nil {
			persistErrs = append(persistErrs, fmt.Errorf("persist %s: %w", set.Codec, err))
			if ferr := database.FailSegmentSet(ctx, row.ID); ferr != nil {
				log.Printf("    failed to mark segment set %d failed: %v", row.ID, ferr)
			}
		}
	}
	if len(persistErrs) > 0 {
		return errors.Join(persistErrs...)
	}

	return nil
}
