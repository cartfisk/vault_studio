# Gapless Lossless Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce ALAC and FLAC fragmented-MP4 renditions of every lossless upload, record their fragment byte layout, and expose both to a client through the existing stream-url endpoint.

**Architecture:** On upload, a second transcoding job encodes the source to one fragmented MP4 per lossless codec. A Go MP4 box-walker records where each fragment begins; those byte ranges plus the track's true sample count go in two new SQLite tables. The client asks for a stream URL with the codecs it supports and gets back a manifest of byte ranges, which it fetches with HTTP `Range` against a new authenticated endpoint.

**Tech Stack:** Go 1.25 (toolchain 1.26), SQLite via mattn/go-sqlite3, sqlc for query codegen, ffmpeg 8.x, `net/http` `ServeMux` with Go 1.22 path patterns.

**Spec:** `docs/superpowers/specs/2026-08-25-gapless-lossless-backend-design.md`. Read it before starting. Its "Discovered while designing" section explains three things the code will otherwise mislead you about.

## Global Constraints

- **Never cut audio with `ffmpeg -c copy`.** It cuts at packet boundaries, not sample boundaries, and silently corrupts sample-exact work. Remuxing a whole file without `-ss`/`-t` is a different operation and is safe — that is the only place `-c:a copy` appears in this plan.
- **Fragment layout is measured, never assumed.** Every byte offset stored comes from parsing the produced file.
- **`track_files` is not modified.** No schema change, no row migration, no edits to the stats query or to `findTrackFile`.
- **The websocket contract does not change.** Only the existing lossy job calls `NotifyTranscodingUpdate`.
- **`GET /api/media/stream/{id}` without a `codecs` parameter must return a byte-identical response to today's.**
- **Fragment duration is `10000000` microseconds (10s).** Declare it once as a named constant.
- **Codec identifiers are exactly `alac` and `flac`,** lowercase, everywhere — DB `CHECK`, query parameters, JSON.
- **Go tests run with `go test ./...` from the repo root.** There is no `make test` target despite the `.PHONY` line.

## Prerequisites

`sqlc` is not on `PATH` in this worktree and is not registered as a `go tool` in `go.mod` (only `wgo` is), even though generated code under `internal/db/sqlc/` is checked in. Before Task 1:

```bash
brew install sqlc
```

Do not add sqlc to `go.mod` as a tool — that is a repo-wide change outside this plan's scope. If the team later wants it, that is a separate change.

Verify ffmpeg is present (8.1.2 confirmed in this environment):

```bash
ffmpeg -version | head -1
```

---

## File Structure

**Create:**

| Path | Responsibility |
|---|---|
| `migrations/036_add_gapless_segments.sql` | The two new tables and their index |
| `internal/db/queries/segments.sql` | sqlc query definitions for both tables |
| `internal/testutil/db.go` | Temp-directory `*db.DB` for tests, used by two packages |
| `internal/transcoding/mp4frag.go` | MP4 top-level box walker; pure, no ffmpeg, no I/O beyond `io.ReaderAt` |
| `internal/transcoding/mp4frag_test.go` | Box-walker tests against hand-built byte slices |
| `internal/transcoding/segments.go` | Encode/remux one codec, probe it, walk it, return a result struct |
| `internal/transcoding/segments_test.go` | Pipeline tests; skip when ffmpeg is absent |
| `internal/handlers/gapless.go` | `GET /api/stream/{id}/gapless/{codec}` byte-serving handler |
| `internal/handlers/gapless_test.go` | Access-control and Range tests |
| `cmd/generate-segments/main.go` | One-shot backfill and repair command |

**Modify:**

| Path | Change |
|---|---|
| `internal/transcoding/metadata.go:108` | Export `isLosslessCodec` as `IsLosslessCodec` |
| `internal/transcoding/transcoder.go` | `Job.Kind`; `processJob` dispatch; `TranscodeVersion` creates sets and queues the segments job |
| `internal/handlers/media.go` | `MediaHandler` gains `*db.DB`; `StreamURL` grows `codecs` handling and the `gapless` block |
| `internal/handlers/media_test.go` | New file in practice, but grouped here as the `StreamURL` tests |
| `cmd/server/main.go:241` | Pass `database` to `NewMediaHandler` |
| `cmd/server/main.go:376` | Register the gapless bytes route |

---

## Task 0: Prove byte-range appending on hardware

This is a gate, not a formality. Byte-range appending is the one unproven assumption in the design. **If it fails, stop and revise the spec — do not proceed to Task 1.**

**Files:**
- Modify: `frontend/public/mse-spike/index.html`

**Interfaces:**
- Consumes: nothing
- Produces: a recorded pass/fail that authorizes the rest of the plan

- [ ] **Step 1: Generate the fixtures**

```bash
cd frontend/public/mse-spike && ./make-fixtures.sh
```

Expected: `fixtures/big-alac.mp4` exists (240s of pink noise, several tens of MB — incompressible on purpose, to approximate a real lossless track's buffer footprint).

- [ ] **Step 2: Add a range-append mode to the harness**

Add to the `MODES` object in `index.html`:

```js
'alac-ranges':   { type: ALAC, files: ['big-alac.mp4'], strategy: 'ranges' },
```

Add the strategy implementation alongside the existing ones.

**This step's original snippet was wrong and was corrected during execution.**
It placed `audio.play()` after the append loop, so `audio.currentTime` stayed 0
for the loop's whole life and the eviction predicate
`sb.buffered.start(0) < t - 30` reduced to `buffered.start(0) < -30` — never true
on a non-negative timeline. Eviction never fired, the loop appended all 37MB into
a far smaller quota, and the device run failed with `QuotaExceededError` at
15728640 bytes. The mode was measuring "append the whole file at once", which is
the failure this design already knew about, rather than progressive append with
eviction.

The shipped implementation lives in `frontend/public/mse-spike/index.html` under
`if (strategy === 'ranges')`. Read it there rather than reproducing it here. Its
shape:

- `CHUNK` of 1MB, `LEAD_SECONDS` of 30, `START_CHUNKS` of 3.
- Append `START_CHUNKS` chunks, then `await audio.play()` — playback runs before
  the steady-state loop begins.
- Before each later append, wait while `bufferedEnd - currentTime > LEAD_SECONDS`,
  yielding on `timeupdate` rather than spinning.
- Evict everything more than `LEAD_SECONDS` behind the playhead, which now fires
  because `currentTime` advances.
- `ms.endOfStream()` once, after the final append settles.
- Log `buffered end` on the `ended` event, since that value is the pass condition.

Every `appendBuffer` and `remove` is followed by its `updateend` before the next
operation; the two must never overlap.

The general lesson, which applies to the client spec too: a buffer-management
test that appends faster than it plays is not testing buffer management.

- [ ] **Step 3: Serve the harness and run on the target device**

```bash
cd frontend && npm run dev
```

Open `http://<host>:3000/mse-spike/index.html` — the explicit `index.html` is required, the bare directory path is swallowed by the TanStack SPA fallback. Run `alac-ranges` on the same physical device the earlier spike results came from.

- [ ] **Step 4: Check the pass conditions**

Both must hold:

1. No `QuotaExceededError` in the log, and playback reaches the end.
2. Final `buffered end` equals 240.0 within a millisecond.

Recall from the spike: `range count: 1` does **not** prove a clean result. A buffer with 77ms of padding also reports one range. The `buffered end` comparison is the real check.

- [ ] **Step 5: Add a two-track range join**

Add:

```js
'alac-range-join': { type: ALAC, files: ['h1-alac.mp4', 'h2-alac.mp4'], strategy: 'range-join' },
'flac-range-join': { type: FLAC, files: ['h1-flac.mp4', 'h2-flac.mp4'], strategy: 'range-join' },
```

`range-join` does what `exact` does — `sb.mode = 'segments'`, `sb.timestampOffset = i * fixtures.trueDurationPerHalf` — but fetches each file in two Range requests instead of one whole-file fetch. This proves the join survives the delivery change; the spike already proved the join itself.

- [ ] **Step 6: Verify the join**

Run both modes. Pass: no audible click at the boundary, and `buffered end` equals 2.0.

- [ ] **Step 7: Commit the harness changes**

```bash
git add frontend/public/mse-spike/index.html
git commit -m "Add byte-range append modes to the MSE harness

- range-append over big-alac.mp4 with eviction behind the playhead
- two-track range join for ALAC and FLAC
- verifies the delivery change the backend spec depends on"
```

---

## Task 1: Schema, queries, and a test database helper

**Files:**
- Create: `migrations/036_add_gapless_segments.sql`
- Create: `internal/db/queries/segments.sql`
- Create: `internal/testutil/db.go`
- Create: `internal/db/segments_test.go`

**Interfaces:**
- Consumes: `db.New(db.Config{DataDir, DBFile, MigrationsPath})` from `internal/db/db.go`
- Produces:
  - Tables `track_segment_sets`, `track_segment_fragments`
  - `testutil.NewDB(t *testing.T) *db.DB`
  - sqlc methods: `CreateSegmentSet`, `CompleteSegmentSet`, `FailSegmentSet`, `DeleteSegmentFragments`, `CreateSegmentFragment`, `GetCompletedSegmentSet`, `ListSegmentFragments`, `ListLosslessVersionsMissingSegments`

- [ ] **Step 1: Write the migration**

Create `migrations/036_add_gapless_segments.sql`:

```sql
CREATE TABLE track_segment_sets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    version_id INTEGER NOT NULL REFERENCES track_versions(id) ON DELETE CASCADE,
    codec TEXT NOT NULL CHECK (codec IN ('alac', 'flac')),
    file_path TEXT NOT NULL,
    file_size INTEGER NOT NULL DEFAULT 0,
    sample_rate INTEGER NOT NULL DEFAULT 0,
    sample_count INTEGER NOT NULL DEFAULT 0,
    channels INTEGER NOT NULL DEFAULT 0,
    init_byte_end INTEGER NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (version_id, codec)
);

CREATE TABLE track_segment_fragments (
    set_id INTEGER NOT NULL REFERENCES track_segment_sets(id) ON DELETE CASCADE,
    idx INTEGER NOT NULL,
    byte_start INTEGER NOT NULL,
    byte_end INTEGER NOT NULL,
    PRIMARY KEY (set_id, idx),
    CHECK (byte_end >= byte_start)
);

CREATE INDEX idx_track_segment_sets_version
ON track_segment_sets(version_id, status);
```

- [ ] **Step 2: Write the test helper**

Create `internal/testutil/db.go`:

```go
// Package testutil provides shared test fixtures. Not imported by production code.
package testutil

import (
	"path/filepath"
	"runtime"
	"testing"

	"bungleware/vault/internal/db"
)

// NewDB opens a migrated SQLite database in a per-test temp directory.
func NewDB(t *testing.T) *db.DB {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve testutil source path")
	}
	migrations := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	database, err := db.New(db.Config{
		DataDir:        t.TempDir(),
		DBFile:         "test.db",
		MigrationsPath: migrations,
	})
	if err != nil {
		t.Fatalf("db.New() error = %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return database
}
```

Resolving migrations from the source file's own location rather than a relative path means the helper works from any package's test directory.

- [ ] **Step 3: Write the failing schema test**

Create `internal/db/segments_test.go`:

```go
package db_test

import (
	"testing"

	"bungleware/vault/internal/testutil"
)

func TestSegmentSetsUniquePerVersionAndCodec(t *testing.T) {
	database := testutil.NewDB(t)

	mustSeedVersion(t, database, 1)

	insert := `INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`
	if _, err := database.Exec(insert, 1, "alac", "/tmp/a.mp4"); err != nil {
		t.Fatalf("first insert error = %v", err)
	}
	if _, err := database.Exec(insert, 1, "flac", "/tmp/f.mp4"); err != nil {
		t.Fatalf("second codec insert error = %v", err)
	}
	if _, err := database.Exec(insert, 1, "alac", "/tmp/dup.mp4"); err == nil {
		t.Fatal("duplicate (version_id, codec) was accepted, want constraint violation")
	}
}

func TestSegmentSetRejectsUnknownCodec(t *testing.T) {
	database := testutil.NewDB(t)
	mustSeedVersion(t, database, 1)

	_, err := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`,
		1, "aac", "/tmp/a.mp4",
	)
	if err == nil {
		t.Fatal("codec 'aac' was accepted, want CHECK violation")
	}
}

func TestFragmentsCascadeOnSetDelete(t *testing.T) {
	database := testutil.NewDB(t)
	mustSeedVersion(t, database, 1)

	res, err := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`,
		1, "alac", "/tmp/a.mp4",
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

func TestFragmentRejectsInvertedRange(t *testing.T) {
	database := testutil.NewDB(t)
	mustSeedVersion(t, database, 1)

	res, _ := database.Exec(
		`INSERT INTO track_segment_sets (version_id, codec, file_path) VALUES (?, ?, ?)`,
		1, "alac", "/tmp/a.mp4",
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

// mustSeedVersion creates the minimum rows a track_version foreign key needs.
func mustSeedVersion(t *testing.T, database interface {
	Exec(string, ...any) (sql.Result, error)
}, versionID int64) {
	t.Helper()
	// Implemented in Step 4 once the exact parent-table columns are confirmed.
}
```

`mustSeedVersion` is finished in the next step — writing it requires reading the parent tables' `NOT NULL` columns, which is a separate action.

- [ ] **Step 4: Finish the seed helper**

Read the parent schemas:

```bash
awk '/CREATE TABLE (projects|tracks|track_versions)/,/^\);/' migrations/001_initial_schema.sql
```

Replace the stub with a real implementation that inserts a project, a track, and a version, supplying every `NOT NULL` column those tables declare. Add `"database/sql"` to the imports. Change the parameter type to `*db.DB` and import `bungleware/vault/internal/db`.

- [ ] **Step 5: Run the tests to verify they fail**

```bash
go test ./internal/db/ -run TestSegment -v
```

Expected: FAIL — `no such table: track_segment_sets`, because the migration has not been applied to a fresh temp database yet. If it passes here, the migration file is being picked up and you can skip to Step 6.

- [ ] **Step 6: Run again to confirm the migration applies**

The migration runs automatically inside `db.New`. Re-run:

```bash
go test ./internal/db/ -run TestSegment -v
```

Expected: PASS, all four tests.

- [ ] **Step 7: Write the sqlc queries**

Create `internal/db/queries/segments.sql`:

```sql
-- name: CreateSegmentSet :one
INSERT INTO track_segment_sets (version_id, codec, file_path, status)
VALUES (?, ?, ?, 'pending')
ON CONFLICT(version_id, codec) DO UPDATE SET
    file_path = excluded.file_path,
    status = 'pending',
    file_size = 0,
    sample_rate = 0,
    sample_count = 0,
    channels = 0,
    init_byte_end = 0
RETURNING *;

-- name: CompleteSegmentSet :exec
UPDATE track_segment_sets
SET file_size = ?, sample_rate = ?, sample_count = ?, channels = ?,
    init_byte_end = ?, status = 'completed'
WHERE id = ?;

-- name: FailSegmentSet :exec
UPDATE track_segment_sets SET status = 'failed' WHERE id = ?;

-- name: DeleteSegmentFragments :exec
DELETE FROM track_segment_fragments WHERE set_id = ?;

-- name: CreateSegmentFragment :exec
INSERT INTO track_segment_fragments (set_id, idx, byte_start, byte_end)
VALUES (?, ?, ?, ?);

-- name: GetCompletedSegmentSet :one
SELECT * FROM track_segment_sets
WHERE version_id = ? AND codec = ? AND status = 'completed';

-- name: ListSegmentFragments :many
SELECT idx, byte_start, byte_end FROM track_segment_fragments
WHERE set_id = ? ORDER BY idx;

-- name: ListLosslessVersionsMissingSegments :many
SELECT tv.id AS version_id, tf.file_path AS source_path
FROM track_versions tv
JOIN track_files tf
    ON tf.version_id = tv.id AND tf.quality = 'source'
WHERE NOT EXISTS (
    SELECT 1 FROM track_segment_sets s
    WHERE s.version_id = tv.id
      AND s.codec = 'alac'
      AND s.status = 'completed'
)
ORDER BY tv.id;
```

The backfill query keys on the ALAC set only. Both codecs are written by one job, so an ALAC set at `completed` implies the FLAC set reached the same state.

- [ ] **Step 8: Generate and build**

```bash
sqlc generate && go build ./...
```

Expected: new methods appear in `internal/db/sqlc/segments.sql.go`, build succeeds.

- [ ] **Step 9: Commit**

```bash
git add migrations/036_add_gapless_segments.sql internal/db/queries/segments.sql \
        internal/db/sqlc internal/testutil/db.go internal/db/segments_test.go
git commit -m "Add segment set and fragment tables

- track_segment_sets: one row per version per lossless codec
- track_segment_fragments: measured byte ranges, end inclusive
- testutil.NewDB for migrated temp databases in tests
- sqlc queries for the transcoder, the API, and backfill"
```

---

## Task 2: MP4 fragment box walker

**Files:**
- Create: `internal/transcoding/mp4frag.go`
- Create: `internal/transcoding/mp4frag_test.go`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `type Fragment struct { Start, End int64 }` — both inclusive
  - `type FragmentLayout struct { InitByteEnd int64; Fragments []Fragment }`
  - `func ScanFragments(r io.ReaderAt, size int64) (FragmentLayout, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/transcoding/mp4frag_test.go`:

```go
package transcoding

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// box builds a 32-bit-size MP4 box.
func box(typ string, payloadLen int) []byte {
	b := make([]byte, 8+payloadLen)
	binary.BigEndian.PutUint32(b[0:4], uint32(8+payloadLen))
	copy(b[4:8], typ)
	return b
}

// largeBox builds a 64-bit-largesize MP4 box (size field == 1).
func largeBox(typ string, payloadLen int) []byte {
	b := make([]byte, 16+payloadLen)
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+payloadLen))
	return b
}

// ftyp(16) moov(16) | moof(16) mdat(40) | moof(16) mdat(40)
func twoFragmentFile() []byte {
	var buf bytes.Buffer
	buf.Write(box("ftyp", 8))
	buf.Write(box("moov", 8))
	buf.Write(box("moof", 8))
	buf.Write(box("mdat", 32))
	buf.Write(box("moof", 8))
	buf.Write(box("mdat", 32))
	return buf.Bytes()
}

func TestScanFragmentsTwoFragments(t *testing.T) {
	data := twoFragmentFile()
	got, err := ScanFragments(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ScanFragments() error = %v", err)
	}

	if got.InitByteEnd != 31 {
		t.Errorf("InitByteEnd = %d, want 31", got.InitByteEnd)
	}
	want := []Fragment{{Start: 32, End: 87}, {Start: 88, End: 143}}
	if len(got.Fragments) != len(want) {
		t.Fatalf("Fragments = %d, want %d", len(got.Fragments), len(want))
	}
	for i := range want {
		if got.Fragments[i] != want[i] {
			t.Errorf("Fragments[%d] = %+v, want %+v", i, got.Fragments[i], want[i])
		}
	}
}

func TestScanFragmentsLastFragmentEndsAtEOF(t *testing.T) {
	data := twoFragmentFile()
	got, err := ScanFragments(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ScanFragments() error = %v", err)
	}
	last := got.Fragments[len(got.Fragments)-1]
	if last.End != int64(len(data))-1 {
		t.Errorf("last fragment End = %d, want %d", last.End, len(data)-1)
	}
}

func TestScanFragmentsLargeSizeBox(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(box("ftyp", 8))
	buf.Write(box("moov", 8))
	buf.Write(box("moof", 8))
	buf.Write(largeBox("mdat", 32)) // 48 bytes
	data := buf.Bytes()

	got, err := ScanFragments(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("ScanFragments() error = %v", err)
	}
	if len(got.Fragments) != 1 {
		t.Fatalf("Fragments = %d, want 1", len(got.Fragments))
	}
	if got.Fragments[0] != (Fragment{Start: 32, End: 95}) {
		t.Errorf("Fragments[0] = %+v, want {32 95}", got.Fragments[0])
	}
}

func TestScanFragmentsTruncatedFile(t *testing.T) {
	data := twoFragmentFile()
	truncated := data[:len(data)-10] // final mdat claims more bytes than exist

	if _, err := ScanFragments(bytes.NewReader(truncated), int64(len(truncated))); err == nil {
		t.Fatal("ScanFragments() error = nil, want error for truncated file")
	}
}

func TestScanFragmentsNoFragments(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(box("ftyp", 8))
	buf.Write(box("moov", 8))
	data := buf.Bytes()

	if _, err := ScanFragments(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("ScanFragments() error = nil, want error when no moof is present")
	}
}

func TestScanFragmentsUndersizedBox(t *testing.T) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], 4) // smaller than the 8-byte header
	copy(b[4:8], "ftyp")

	if _, err := ScanFragments(bytes.NewReader(b), int64(len(b))); err == nil {
		t.Fatal("ScanFragments() error = nil, want error for undersized box")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/transcoding/ -run TestScanFragments -v
```

Expected: FAIL — `undefined: ScanFragments`.

- [ ] **Step 3: Implement the walker**

Create `internal/transcoding/mp4frag.go`:

```go
package transcoding

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Fragment is one moof/mdat pair, as an inclusive byte range.
// Inclusive because it maps directly onto HTTP "Range: bytes=start-end".
type Fragment struct {
	Start int64
	End   int64
}

// FragmentLayout is the measured structure of a fragmented MP4.
// InitByteEnd is the inclusive end of the ftyp+moov prelude, which a client
// appends once before any fragment.
type FragmentLayout struct {
	InitByteEnd int64
	Fragments   []Fragment
}

// ScanFragments walks the top-level boxes of a fragmented MP4 and records
// where each moof begins. It reports the layout the file actually has, never
// the layout the encoder was asked for.
func ScanFragments(r io.ReaderAt, size int64) (FragmentLayout, error) {
	var layout FragmentLayout
	var moofStarts []int64

	header := make([]byte, 16)
	offset := int64(0)

	for offset < size {
		if size-offset < 8 {
			return layout, fmt.Errorf("truncated box header at offset %d", offset)
		}
		if _, err := r.ReadAt(header[:8], offset); err != nil {
			return layout, fmt.Errorf("read box header at %d: %w", offset, err)
		}

		boxSize := int64(binary.BigEndian.Uint32(header[0:4]))
		boxType := string(header[4:8])

		switch {
		case boxSize == 1:
			if size-offset < 16 {
				return layout, fmt.Errorf("truncated largesize header at offset %d", offset)
			}
			if _, err := r.ReadAt(header[8:16], offset+8); err != nil {
				return layout, fmt.Errorf("read largesize at %d: %w", offset, err)
			}
			boxSize = int64(binary.BigEndian.Uint64(header[8:16]))
			if boxSize < 16 {
				return layout, fmt.Errorf("largesize box at %d claims %d bytes", offset, boxSize)
			}
		case boxSize == 0:
			// Extends to end of file.
			boxSize = size - offset
		case boxSize < 8:
			return layout, fmt.Errorf("box at %d claims %d bytes, minimum is 8", offset, boxSize)
		}

		if offset+boxSize > size {
			return layout, fmt.Errorf(
				"box %q at %d claims %d bytes, past end of file at %d",
				boxType, offset, boxSize, size,
			)
		}

		if boxType == "moof" {
			moofStarts = append(moofStarts, offset)
		}

		offset += boxSize
	}

	if len(moofStarts) == 0 {
		return layout, fmt.Errorf("no moof box found; file is not fragmented")
	}

	layout.InitByteEnd = moofStarts[0] - 1
	layout.Fragments = make([]Fragment, len(moofStarts))
	for i, start := range moofStarts {
		end := size - 1
		if i+1 < len(moofStarts) {
			end = moofStarts[i+1] - 1
		}
		layout.Fragments[i] = Fragment{Start: start, End: end}
	}

	return layout, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/transcoding/ -run TestScanFragments -v
```

Expected: PASS, all six tests.

- [ ] **Step 5: Commit**

```bash
git add internal/transcoding/mp4frag.go internal/transcoding/mp4frag_test.go
git commit -m "Add fragmented MP4 box walker

- ScanFragments reports the layout a file actually has
- handles 64-bit largesize and size-to-EOF boxes
- errors rather than returning partial offsets on truncation"
```

---

## Task 3: Segment set builder

**Files:**
- Create: `internal/transcoding/segments.go`
- Create: `internal/transcoding/segments_test.go`
- Modify: `internal/transcoding/metadata.go:108`

**Interfaces:**
- Consumes: `ScanFragments`, `FragmentLayout`, `Fragment` from Task 2
- Produces:
  - `const FragmentDurationMicros = 10000000`
  - `func IsLosslessCodec(codec string) bool` — the exported rename
  - `type SegmentSet struct { Codec, FilePath string; FileSize int64; SampleRate, Channels int; SampleCount int64; Layout FragmentLayout }`
  - `func BuildSegmentSet(sourcePath, outputPath, targetCodec, sourceCodec string) (*SegmentSet, error)`
  - `func BuildAllSegmentSets(sourcePath, versionDir, sourceCodec string) ([]*SegmentSet, error)`

- [ ] **Step 1: Export the lossless predicate**

In `internal/transcoding/metadata.go`, rename `isLosslessCodec` to `IsLosslessCodec` and update its one call site at line 100:

```go
metadata.IsLossless = IsLosslessCodec(stream.CodecName)
```

Confirm nothing else referenced the old name:

```bash
grep -rn "isLosslessCodec" --include="*.go" . && echo "FOUND — fix these" || echo "clean"
go build ./...
```

- [ ] **Step 2: Write the failing tests**

Create `internal/transcoding/segments_test.go`:

```go
package transcoding

import (
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
```

Add `"fmt"` to the imports.

- [ ] **Step 3: Run the tests to verify they fail**

```bash
go test ./internal/transcoding/ -run TestBuild -v
```

Expected: FAIL — `undefined: BuildSegmentSet`.

- [ ] **Step 4: Implement the builder**

Create `internal/transcoding/segments.go`:

```go
package transcoding

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// FragmentDurationMicros is the fragment duration requested of ffmpeg.
// The layout actually produced is measured, never assumed to match this.
const FragmentDurationMicros = 10000000

// fragMovFlags is the invocation proven on hardware by the MSE spike.
const fragMovFlags = "+frag_keyframe+empty_moov+default_base_moof"

// SegmentCodecs are the lossless codecs served for gapless playback, one per
// browser engine: ALAC for Safari, FLAC for Chrome and Firefox.
var SegmentCodecs = []string{"alac", "flac"}

// SegmentSet is one produced fragmented MP4 and its measured properties.
type SegmentSet struct {
	Codec       string
	FilePath    string
	FileSize    int64
	SampleRate  int
	Channels    int
	SampleCount int64
	Layout      FragmentLayout
}

// BuildSegmentSet encodes or remuxes sourcePath into a fragmented MP4 at
// outputPath, then measures what it produced.
//
// When sourceCodec already matches targetCodec the audio is stream-copied.
// That is a whole-file remux with no -ss/-t, so it keeps every frame; it is
// not the packet-boundary cutting that corrupts sample-exact work.
func BuildSegmentSet(sourcePath, outputPath, targetCodec, sourceCodec string) (*SegmentSet, error) {
	if targetCodec != "alac" && targetCodec != "flac" {
		return nil, fmt.Errorf("unsupported segment codec %q", targetCodec)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	codecArg := targetCodec
	if sourceCodec == targetCodec {
		codecArg = "copy"
	}

	cmd := exec.Command("ffmpeg", "-v", "error",
		"-i", sourcePath,
		"-vn",
		"-c:a", codecArg,
		"-frag_duration", strconv.Itoa(FragmentDurationMicros),
		"-movflags", fragMovFlags,
		"-f", "mp4",
		"-y", outputPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg %s failed: %w: %s", targetCodec, err, out)
	}

	probe, err := probeSegmentFile(outputPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("open produced file: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat produced file: %w", err)
	}

	layout, err := ScanFragments(f, stat.Size())
	if err != nil {
		return nil, fmt.Errorf("scan %s fragments: %w", targetCodec, err)
	}

	// A track longer than one fragment duration that produced a single
	// fragment was not fragmented. Appending it whole is the case already
	// measured to exhaust the SourceBuffer quota, so refuse it here rather
	// than ship an unplayable set. If this fires, drop +frag_keyframe from
	// fragMovFlags and re-measure — do not relax this check.
	contentMicros := probe.SampleCount * 1000000 / int64(probe.SampleRate)
	if contentMicros > FragmentDurationMicros && len(layout.Fragments) < 2 {
		return nil, fmt.Errorf(
			"%s output is %dus but produced 1 fragment; not fragmented",
			targetCodec, contentMicros,
		)
	}

	return &SegmentSet{
		Codec:       targetCodec,
		FilePath:    outputPath,
		FileSize:    stat.Size(),
		SampleRate:  probe.SampleRate,
		Channels:    probe.Channels,
		SampleCount: probe.SampleCount,
		Layout:      layout,
	}, nil
}

// BuildAllSegmentSets produces one set per codec in SegmentCodecs and refuses
// to return any of them unless they agree on sample count.
//
// The cross-check is why both codecs are built together. A disagreement means
// one encode dropped or padded audio, and placing tracks at running offsets of
// a wrong length is exactly the failure this feature exists to avoid.
func BuildAllSegmentSets(sourcePath, versionDir, sourceCodec string) ([]*SegmentSet, error) {
	sets := make([]*SegmentSet, 0, len(SegmentCodecs))

	for _, codec := range SegmentCodecs {
		out := filepath.Join(versionDir, "gapless-"+codec+".mp4")
		set, err := BuildSegmentSet(sourcePath, out, codec, sourceCodec)
		if err != nil {
			return nil, err
		}
		sets = append(sets, set)
	}

	for _, set := range sets[1:] {
		if set.SampleCount != sets[0].SampleCount {
			return nil, fmt.Errorf(
				"sample count disagreement: %s=%d, %s=%d",
				sets[0].Codec, sets[0].SampleCount, set.Codec, set.SampleCount,
			)
		}
	}

	return sets, nil
}

type segmentProbe struct {
	SampleRate  int
	Channels    int
	SampleCount int64
}

// probeSegmentFile reads the true sample count from a produced file.
//
// duration_ts is in time_base units, so the count is scaled rather than read
// directly: sampleCount = durationTS * tbNum * sampleRate / tbDen. For these
// files the time base is normally 1/sampleRate and the scaling is a no-op, but
// relying on that would be an assumption about the muxer.
func probeSegmentFile(path string) (segmentProbe, error) {
	var p segmentProbe

	cmd := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=duration_ts,time_base,sample_rate,channels",
		"-of", "json", path,
	)
	out, err := cmd.Output()
	if err != nil {
		return p, fmt.Errorf("ffprobe %s: %w", path, err)
	}

	var parsed struct {
		Streams []struct {
			DurationTS int64  `json:"duration_ts"`
			TimeBase   string `json:"time_base"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return p, fmt.Errorf("parse ffprobe output for %s: %w", path, err)
	}
	if len(parsed.Streams) == 0 {
		return p, fmt.Errorf("no audio stream in %s", path)
	}
	s := parsed.Streams[0]

	rate, err := strconv.Atoi(s.SampleRate)
	if err != nil || rate <= 0 {
		return p, fmt.Errorf("unusable sample_rate %q in %s", s.SampleRate, path)
	}

	num, den, err := parseTimeBase(s.TimeBase)
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}

	p.SampleRate = rate
	p.Channels = s.Channels
	p.SampleCount = s.DurationTS * num * int64(rate) / den

	if p.SampleCount <= 0 {
		return p, fmt.Errorf("computed sample count %d for %s", p.SampleCount, path)
	}

	return p, nil
}

func parseTimeBase(tb string) (num, den int64, err error) {
	parts := strings.SplitN(tb, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unparseable time_base %q", tb)
	}
	num, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable time_base numerator in %q", tb)
	}
	den, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || den == 0 {
		return 0, 0, fmt.Errorf("unparseable time_base denominator in %q", tb)
	}
	return num, den, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/transcoding/ -run "TestBuild|TestScanFragments" -v
```

Expected: PASS. If `TestBuildSegmentSetFragmentsALongTrack` fails with the "not fragmented" error, that is the `+frag_keyframe` interaction the constant's comment warns about: remove `+frag_keyframe` from `fragMovFlags`, re-run, and record the change in the spec.

- [ ] **Step 6: Commit**

```bash
git add internal/transcoding/segments.go internal/transcoding/segments_test.go \
        internal/transcoding/metadata.go
git commit -m "Add lossless segment set builder

- BuildSegmentSet encodes or remuxes to fragmented MP4, then measures it
- stream copy when the source codec already matches the target
- BuildAllSegmentSets refuses sets whose sample counts disagree
- reject a long track that produced a single fragment
- export IsLosslessCodec for the upload path"
```

---

## Task 4: Wire segment generation into the transcoder

**Files:**
- Modify: `internal/transcoding/transcoder.go`
- Create: `internal/transcoding/transcoder_segments_test.go`

**Interfaces:**
- Consumes: `BuildAllSegmentSets`, `IsLosslessCodec`, `SegmentCodecs` from Task 3; `CreateSegmentSet`, `CompleteSegmentSet`, `FailSegmentSet`, `DeleteSegmentFragments`, `CreateSegmentFragment` from Task 1
- Produces: `Job.Kind` field; `JobKindLossy`, `JobKindSegments` constants

- [ ] **Step 1: Write the failing test**

Create `internal/transcoding/transcoder_segments_test.go`:

```go
package transcoding_test

import (
	"context"
	"testing"

	"bungleware/vault/internal/testutil"
	"bungleware/vault/internal/transcoding"
)

func TestTranscodeVersionSkipsSegmentsForLossySource(t *testing.T) {
	database := testutil.NewDB(t)
	// Seed a project, track, and version exactly as internal/db/segments_test.go
	// does. Reuse that helper by lifting it into internal/testutil in this step.
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
```

Move `mustSeedVersion` from `internal/db/segments_test.go` into `internal/testutil` as an exported `SeedVersion(t *testing.T, database *db.DB) int64` returning the new version id, and update the Task 1 tests to call it. Two packages need it now, which is what justifies the move.

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/transcoding/ -run TestTranscodeVersion -v
```

Expected: FAIL — `unknown field SourceCodec` and `undefined: testutil.SeedVersion`.

- [ ] **Step 3: Add the job kind and the source codec input**

In `internal/transcoding/transcoder.go`:

```go
const (
	JobKindLossy    = "lossy"
	JobKindSegments = "segments"
)

type Job struct {
	Kind          string
	TrackFileID   int64
	VersionID     int64
	TrackPublicID string
	UserID        int64
	SourcePath    string
	OutputPath    string
	SourceCodec   string
}
```

Add `SourceCodec string` to `TranscodeVersionInput`.

- [ ] **Step 4: Dispatch on kind in the worker**

Replace the body of `processJob` with a dispatch, keeping the existing MP3 path intact and unchanged:

```go
func (t *Transcoder) processJob(job Job) {
	switch job.Kind {
	case JobKindSegments:
		t.processSegmentsJob(job)
	default:
		t.processLossyJob(job)
	}
}
```

Rename the current `processJob` body to `processLossyJob`. Do not otherwise modify it — it owns the websocket notification and the MP3 status transitions, and both must stay exactly as they are.

- [ ] **Step 5: Implement the segments job**

Add to `internal/transcoding/transcoder.go`:

```go
// processSegmentsJob builds both lossless sets and records their layout.
// It never notifies over the websocket: the client discovers gapless
// availability when it requests a stream URL, which keeps the existing
// notification contract untouched.
func (t *Transcoder) processSegmentsJob(job Job) {
	ctx := context.Background()

	setIDs, err := t.segmentSetIDs(ctx, job.VersionID)
	if err != nil {
		log.Printf("Segment sets missing for version %d: %v", job.VersionID, err)
		return
	}

	for _, id := range setIDs {
		if err := t.db.MarkSegmentSetProcessing(ctx, id); err != nil {
			log.Printf("Failed to mark segment set %d processing: %v", id, err)
		}
	}

	versionDir := filepath.Dir(job.SourcePath)
	sets, err := BuildAllSegmentSets(job.SourcePath, versionDir, job.SourceCodec)
	if err != nil {
		log.Printf("Segment generation failed for version %d: %v", job.VersionID, err)
		for _, id := range setIDs {
			if ferr := t.db.FailSegmentSet(ctx, id); ferr != nil {
				log.Printf("Failed to mark segment set %d failed: %v", id, ferr)
			}
		}
		return
	}

	for _, set := range sets {
		id, ok := setIDs[set.Codec]
		if !ok {
			log.Printf("No row for codec %s on version %d", set.Codec, job.VersionID)
			continue
		}
		if err := t.persistSegmentSet(ctx, id, set); err != nil {
			log.Printf("Failed to persist %s set for version %d: %v", set.Codec, job.VersionID, err)
			if ferr := t.db.FailSegmentSet(ctx, id); ferr != nil {
				log.Printf("Failed to mark segment set %d failed: %v", id, ferr)
			}
		}
	}

	log.Printf("Generated lossless segment sets for version %d", job.VersionID)
}

func (t *Transcoder) persistSegmentSet(ctx context.Context, setID int64, set *SegmentSet) error {
	if err := t.db.DeleteSegmentFragments(ctx, setID); err != nil {
		return fmt.Errorf("clear fragments: %w", err)
	}

	for i, frag := range set.Layout.Fragments {
		err := t.db.CreateSegmentFragment(ctx, sqlc.CreateSegmentFragmentParams{
			SetID:     setID,
			Idx:       int64(i),
			ByteStart: frag.Start,
			ByteEnd:   frag.End,
		})
		if err != nil {
			return fmt.Errorf("insert fragment %d: %w", i, err)
		}
	}

	return t.db.CompleteSegmentSet(ctx, sqlc.CompleteSegmentSetParams{
		FileSize:    set.FileSize,
		SampleRate:  int64(set.SampleRate),
		SampleCount: set.SampleCount,
		Channels:    int64(set.Channels),
		InitByteEnd: set.Layout.InitByteEnd,
		ID:          setID,
	})
}
```

Add the lookup this depends on:

```go
// segmentSetIDs maps codec to the row id created by TranscodeVersion.
func (t *Transcoder) segmentSetIDs(ctx context.Context, versionID int64) (map[string]int64, error) {
	rows, err := t.db.ListSegmentSetsForVersion(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("list segment sets: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no segment set rows for version %d", versionID)
	}

	ids := make(map[string]int64, len(rows))
	for _, row := range rows {
		ids[row.Codec] = row.ID
	}
	return ids, nil
}
```

`MarkSegmentSetProcessing` and `ListSegmentSetsForVersion` are new sqlc queries — add it to `internal/db/queries/segments.sql` and regenerate:

```sql
-- name: MarkSegmentSetProcessing :exec
UPDATE track_segment_sets SET status = 'processing' WHERE id = ?;

-- name: ListSegmentSetsForVersion :many
SELECT id, codec FROM track_segment_sets WHERE version_id = ?;
```

- [ ] **Step 6: Create the sets and queue the job**

At the end of `TranscodeVersion`, after the existing MP3 `QueueJob` call:

```go
	if !IsLosslessCodec(input.SourceCodec) {
		return nil
	}

	for _, codec := range SegmentCodecs {
		outPath := filepath.Join(sourceDir, "gapless-"+codec+".mp4")
		if _, err := t.db.CreateSegmentSet(ctx, sqlc.CreateSegmentSetParams{
			VersionID: input.VersionID,
			Codec:     codec,
			FilePath:  outPath,
		}); err != nil {
			return fmt.Errorf("failed to create %s segment set: %w", codec, err)
		}
	}

	t.QueueJob(Job{
		Kind:        JobKindSegments,
		VersionID:   input.VersionID,
		SourcePath:  input.SourceFilePath,
		SourceCodec: input.SourceCodec,
	})

	return nil
```

Set `Kind: JobKindLossy` on the existing MP3 `QueueJob` call so both are explicit.

- [ ] **Step 7: Pass the source codec from the upload handler**

In `internal/handlers/tracks/upload.go`, the `TranscodeVersion` call around line 226 gains the codec already probed into `metadata`:

```go
			SourceCodec:    metadata.Codec,
```

- [ ] **Step 8: Run the tests**

```bash
go test ./internal/transcoding/ -v && go build ./...
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/transcoding/ internal/testutil/ internal/db/ \
        internal/handlers/tracks/upload.go
git commit -m "Generate lossless segment sets on upload

- Job gains Kind; segments job runs alongside the MP3 job
- lossy sources are skipped entirely
- segment failures are logged and never block playback
- websocket notifications stay on the MP3 job only"
```

---

## Task 5: Gapless bytes endpoint

**Files:**
- Create: `internal/handlers/gapless.go`
- Create: `internal/handlers/gapless_test.go`
- Modify: `cmd/server/main.go:376`

**Interfaces:**
- Consumes: `GetCompletedSegmentSet` from Task 1; `tracks.CheckTrackAccess` from `internal/handlers/tracks`
- Produces: `func (h *StreamingHandler) StreamGapless(w http.ResponseWriter, r *http.Request) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/handlers/gapless_test.go`. Cover, at minimum:

```go
func TestStreamGaplessRejectsRevokedAccess(t *testing.T)
func TestStreamGaplessRejectsUnknownCodec(t *testing.T)
func TestStreamGaplessServesRangeRequest(t *testing.T)
func TestStreamGaplessMissingSetReturns404(t *testing.T)
```

The revocation test is the one that matters most — without it a revoked share
stays playable through this path:

```go
func TestStreamGaplessRejectsRevokedAccess(t *testing.T) {
	database := testutil.NewDB(t)
	owner, stranger := int64(1), int64(2)

	// Seed a track owned by `owner`, with a completed ALAC set on disk.
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)
	path := filepath.Join(t.TempDir(), "gapless-alac.mp4")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackPublicID+"/gapless/alac", nil)
	req.SetPathValue("id", trackPublicID)
	req.SetPathValue("codec", "alac")
	req = req.WithContext(middleware.WithUserID(req.Context(), int(stranger)))

	err := h.StreamGapless(httptest.NewRecorder(), req)
	if err == nil {
		t.Fatal("StreamGapless() error = nil for a user without access, want forbidden")
	}
	if status := apperr.StatusOf(err); status != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", status, http.StatusForbidden)
	}
}

func TestStreamGaplessServesRangeRequest(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)

	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 251)
	}
	path := filepath.Join(t.TempDir(), "gapless-alac.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", path)

	h := handlers.NewStreamingHandler(database)

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+trackPublicID+"/gapless/alac", nil)
	req.SetPathValue("id", trackPublicID)
	req.SetPathValue("codec", "alac")
	req.Header.Set("Range", "bytes=1024-2047")
	req = req.WithContext(middleware.WithUserID(req.Context(), int(owner)))

	rec := httptest.NewRecorder()
	if err := h.StreamGapless(rec, req); err != nil {
		t.Fatalf("StreamGapless() error = %v", err)
	}

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusPartialContent)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) != 1024 {
		t.Fatalf("body length = %d, want 1024", len(body))
	}
	if !bytes.Equal(body, data[1024:2048]) {
		t.Error("body does not match the requested byte range")
	}
	if ct := res.Header.Get("Content-Type"); ct != "audio/mp4" {
		t.Errorf("Content-Type = %q, want \"audio/mp4\"", ct)
	}
}
```

Three helpers this needs, added to `internal/testutil` in this step:

- `SeedTrackForUser(t, database, userID) (publicID string, versionID int64)` — a project, a track, and a version owned by `userID`, with a `source` track file row. Extends the `SeedVersion` helper from Task 4 rather than replacing it.
- `SeedCompletedSegmentSet(t, database, versionID, codec, path)` — a `completed` set with one fragment covering the whole file.
- `middleware.WithUserID(ctx, id)` — if no exported setter exists beside `middleware.GetUserID`, add one. Tests cannot inject the user otherwise, and an unexported context key is not reachable from `handlers`.

Confirm how `apperr` reports status before writing the assertion:

```bash
grep -n "func.*Status\|StatusCode\|type AppError" internal/apperr/*.go
```

If there is no `StatusOf`, assert on the concrete error type that `NewForbidden` returns instead.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/handlers/ -run TestStreamGapless -v
```

Expected: FAIL — `undefined: StreamGapless`.

- [ ] **Step 3: Implement the handler**

Create `internal/handlers/gapless.go`:

```go
package handlers

import (
	"net/http"
	"os"
	"strconv"

	"bungleware/vault/internal/apperr"
	sqlc "bungleware/vault/internal/db/sqlc"
	"bungleware/vault/internal/handlers/tracks"
	"bungleware/vault/internal/httputil"
	"bungleware/vault/internal/middleware"
)

// StreamGapless serves a lossless fragmented MP4 by byte range.
//
// Unlike StreamTrack this route is not signed: MSE fetches through the app's
// own fetch(), which carries the session cookie on web and the bearer token in
// the Capacitor build. It therefore runs behind normal AuthMiddleware.
func (h *StreamingHandler) StreamGapless(w http.ResponseWriter, r *http.Request) error {
	userID, ok := middleware.GetUserID(r.Context())
	if !ok {
		return apperr.NewUnauthorized("unauthorized")
	}

	codec := r.PathValue("codec")
	if codec != "alac" && codec != "flac" {
		return apperr.NewBadRequest("unsupported codec")
	}

	publicID := r.PathValue("id")
	ctx := r.Context()

	track, err := h.db.Queries.GetTrackByPublicIDNoFilter(ctx, publicID)
	if err := httputil.HandleDBError(err, "track not found", "failed to query track"); err != nil {
		return err
	}

	// Same check StreamTrack performs. Omitting it would let a revoked share
	// keep playing through the gapless path.
	access, err := tracks.CheckTrackAccess(ctx, h.db, track.ID, track.ProjectID, int64(userID))
	if err != nil {
		return apperr.NewInternal("failed to check track access", err)
	}
	if !access.HasAccess {
		return apperr.NewForbidden("access revoked")
	}

	versionID := track.ActiveVersionID.Int64
	if raw := r.URL.Query().Get("version_id"); raw != "" {
		parsed, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			return apperr.NewBadRequest("invalid version_id")
		}
		versionID = parsed
	}
	if versionID == 0 {
		return apperr.NewBadRequest("track has no active version")
	}

	set, err := h.db.GetCompletedSegmentSet(ctx, sqlc.GetCompletedSegmentSetParams{
		VersionID: versionID,
		Codec:     codec,
	})
	if err := httputil.HandleDBError(err, "segment set not found", "failed to query segment set"); err != nil {
		return err
	}

	f, err := os.Open(set.FilePath)
	if err != nil {
		return apperr.NewInternal("failed to open segment file", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return apperr.NewInternal("failed to stat segment file", err)
	}

	w.Header().Set("Content-Type", "audio/mp4")
	// http.ServeContent handles Range, 206, multipart ranges, and malformed
	// range headers. Nothing custom is needed.
	http.ServeContent(w, r, set.FilePath, stat.ModTime(), f)
	return nil
}
```

- [ ] **Step 4: Register the route**

In `cmd/server/main.go`, after line 376:

```go
	mux.Handle("GET /api/stream/{id}/gapless/{codec}", authMW(httputil.Wrap(streamingHandler.StreamGapless)))
```

`authMW`, not `optionalAuthMW(signedURLMW(...))` — this route requires a real session.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/handlers/ -run TestStreamGapless -v && go build ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/handlers/gapless.go internal/handlers/gapless_test.go cmd/server/main.go
git commit -m "Serve lossless segments by byte range

- GET /api/stream/{id}/gapless/{codec} behind normal auth
- same CheckTrackAccess as StreamTrack, with a test for revocation
- Range handling delegated to http.ServeContent"
```

---

## Task 6: Stream-url manifest extension

**Files:**
- Modify: `internal/handlers/media.go`
- Create: `internal/handlers/media_test.go`
- Modify: `cmd/server/main.go:241`

**Interfaces:**
- Consumes: `GetCompletedSegmentSet`, `ListSegmentFragments` from Task 1
- Produces: the `gapless` object in the `StreamURL` response

- [ ] **Step 1: Write the failing tests**

Create `internal/handlers/media_test.go` covering:

```go
func TestStreamURLWithoutCodecsIsUnchanged(t *testing.T)
func TestStreamURLOmitsGaplessWhenNoCompletedSet(t *testing.T)
func TestStreamURLOmitsGaplessForLossyQuality(t *testing.T)
func TestStreamURLPrefersClientCodecOrder(t *testing.T)
```

The compatibility guarantee is what lets the frontend spec ship separately, so
it is asserted on the whole key set, not just on `url` being present:

```go
func TestStreamURLWithoutCodecsIsUnchanged(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)

	// A completed set exists, so any leakage would show up here.
	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", "/tmp/a.mp4")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	req := httptest.NewRequest(http.MethodGet, "/api/media/stream/"+trackPublicID, nil)
	req.SetPathValue("id", trackPublicID)
	req = req.WithContext(middleware.WithUserID(req.Context(), int(owner)))

	rec := httptest.NewRecorder()
	if err := h.StreamURL(rec, req); err != nil {
		t.Fatalf("StreamURL() error = %v", err)
	}

	payload := decodeResultObject(t, rec)

	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) != 1 || keys[0] != "url" {
		t.Fatalf("response keys = %v, want [url]", keys)
	}
}

func TestStreamURLPrefersClientCodecOrder(t *testing.T) {
	database := testutil.NewDB(t)
	owner := int64(1)
	trackPublicID, versionID := testutil.SeedTrackForUser(t, database, owner)

	testutil.SeedCompletedSegmentSet(t, database, versionID, "alac", "/tmp/a.mp4")
	testutil.SeedCompletedSegmentSet(t, database, versionID, "flac", "/tmp/f.mp4")
	testutil.SetUserQuality(t, database, owner, "lossless")

	h := handlers.NewMediaHandler(testAuthConfig(), database)

	for _, tc := range []struct {
		codecs string
		want   string
	}{
		{"flac,alac", "flac"},
		{"alac,flac", "alac"},
		{"flac", "flac"},
	} {
		t.Run(tc.codecs, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet,
				"/api/media/stream/"+trackPublicID+"?codecs="+tc.codecs, nil)
			req.SetPathValue("id", trackPublicID)
			req = req.WithContext(middleware.WithUserID(req.Context(), int(owner)))

			rec := httptest.NewRecorder()
			if err := h.StreamURL(rec, req); err != nil {
				t.Fatalf("StreamURL() error = %v", err)
			}

			payload := decodeResultObject(t, rec)
			gapless, ok := payload["gapless"].(map[string]any)
			if !ok {
				t.Fatalf("gapless missing from response %v", payload)
			}
			if got := gapless["codec"]; got != tc.want {
				t.Errorf("codec = %v, want %q", got, tc.want)
			}
		})
	}
}
```

Two helpers this needs:

- `decodeResultObject(t, rec)` — unmarshals the recorder body and returns the object `httputil.OKResult` wraps. Read `internal/httputil` first to see whether it nests under a `data` or `result` key; the assertion must target the same object `StreamURL` builds, not the envelope.
- `testutil.SetUserQuality(t, database, userID, quality)` — writes the user preference row so `resolveQuality` returns `lossless`. Without it the default is `lossy` and `gapless` is correctly omitted.
- `testAuthConfig()` — an `auth.Config` with a non-empty `SignedURLSecret` and a short expiration, so `BuildSignedURL` returns a URL rather than the empty string.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/handlers/ -run TestStreamURL -v
```

Expected: FAIL — `NewMediaHandler` takes one argument.

- [ ] **Step 3: Give MediaHandler a database**

```go
type MediaHandler struct {
	config auth.Config
	db     *db.DB
}

func NewMediaHandler(config auth.Config, database *db.DB) *MediaHandler {
	return &MediaHandler{config: config, db: database}
}
```

Update `cmd/server/main.go:241`:

```go
	mediaHandler := handlers.NewMediaHandler(config.AuthConfig, database)
```

- [ ] **Step 4: Add the manifest to StreamURL**

Define the response types in `internal/handlers/media.go`:

```go
type gaplessFragment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type gaplessManifest struct {
	Codec       string            `json:"codec"`
	URL         string            `json:"url"`
	SampleRate  int64             `json:"sampleRate"`
	SampleCount int64             `json:"sampleCount"`
	Channels    int64             `json:"channels"`
	InitByteEnd int64             `json:"initByteEnd"`
	Fragments   []gaplessFragment `json:"fragments"`
}
```

Replace the final `return httputil.OKResult(...)` in `StreamURL` with a build that only adds the key when a manifest exists:

```go
	response := map[string]any{"url": url}

	if manifest := h.gaplessManifest(r, trackID, quality, versionID); manifest != nil {
		response["gapless"] = manifest
	}

	return httputil.OKResult(w, response)
```

`gaplessManifest` returns `nil` unless all three conditions hold — a non-empty `codecs` parameter, a resolved quality of `lossless`, and a completed set for one of the requested codecs. It resolves the version the same way `StreamTrack` does, walks the client's codec list in order, and returns on the first hit.

Note the existing handler returns `map[string]string`; widening to `map[string]any` is required and changes nothing about the no-`codecs` response, which still marshals to `{"url":"..."}`.

The quality check must use the same resolution `StreamingHandler.resolveQuality` performs — requested parameter, then project override, then user preference. Extract that method to a shared helper rather than duplicating the precedence, since a divergence between the two would silently offer or hide the feature.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./internal/handlers/ -v && go build ./...
```

Expected: PASS.

- [ ] **Step 6: Verify the compatibility guarantee by hand**

```bash
go run ./cmd/server &
curl -s -b <session cookie> 'http://localhost:8080/api/media/stream/<track>' | jq 'keys'
```

Expected: `["url"]` exactly.

- [ ] **Step 7: Commit**

```bash
git add internal/handlers/media.go internal/handlers/media_test.go \
        internal/handlers/streaming.go cmd/server/main.go
git commit -m "Return a gapless manifest from the stream-url endpoint

- codecs parameter carries the client's isTypeSupported result
- gapless omitted unless quality resolves to lossless and a set exists
- client codec order decides, server availability constrains
- response without codecs is unchanged"
```

---

## Task 7: Backfill command

**Files:**
- Create: `cmd/generate-segments/main.go`

**Interfaces:**
- Consumes: `ListLosslessVersionsMissingSegments`, `CreateSegmentSet` from Task 1; `BuildAllSegmentSets`, `IsLosslessCodec` from Task 3; `persistSegmentSet` logic from Task 4
- Produces: the `generate-segments` binary

- [ ] **Step 1: Write the command**

Create `cmd/generate-segments/main.go` following `cmd/generate-waveforms/main.go`: the same `-data-dir`, `-dry-run`, and `-verbose` flags, the same `db.New` block, the same logging shape.

For each row from `ListLosslessVersionsMissingSegments`:

1. Probe the source with `transcoding.ExtractMetadata`; skip and count when `IsLosslessCodec` is false or the file is missing.
2. In dry-run, log the intent and continue.
3. Otherwise `CreateSegmentSet` for each codec, `BuildAllSegmentSets`, then persist fragments and complete the rows.
4. On error, mark both sets `failed`, log, and continue to the next version. One bad file must not stop the run.

Print a summary: processed, skipped-not-lossless, skipped-missing-source, failed.

The persist step duplicates `persistSegmentSet` from Task 4. Export it from `internal/transcoding` as a function taking a `*db.DB`, a set id, and a `*SegmentSet`, and have the transcoder method call it. Two callers is the threshold that justifies the move; do not extract it earlier.

- [ ] **Step 2: Build**

```bash
go build ./cmd/generate-segments && go build ./...
```

- [ ] **Step 3: Dry-run against the real library**

```bash
./generate-segments -data-dir ./data -dry-run -verbose
```

Expected: lists versions with a lossless source and no completed ALAC set. Writes nothing.

- [ ] **Step 4: Run for real**

```bash
./generate-segments -data-dir ./data -verbose
```

- [ ] **Step 5: Verify idempotency**

```bash
./generate-segments -data-dir ./data -verbose
```

Expected: zero versions processed on the second run — the `NOT EXISTS` clause excludes completed sets. Confirm no duplicate rows:

```bash
sqlite3 data/vault.db \
  'SELECT version_id, codec, COUNT(*) FROM track_segment_sets GROUP BY 1,2 HAVING COUNT(*) > 1;'
```

Expected: no rows.

- [ ] **Step 6: Verify a real track end to end**

```bash
sqlite3 data/vault.db \
  'SELECT file_path, byte_start, byte_end FROM track_segment_sets s
   JOIN track_segment_fragments f ON f.set_id = s.id
   WHERE s.codec = "alac" ORDER BY s.id, f.idx LIMIT 3;'
```

Then request the first fragment through the API and confirm the byte count matches:

```bash
curl -s -b <session cookie> -H 'Range: bytes=<start>-<end>' \
  -o /dev/null -w '%{http_code} %{size_download}\n' \
  'http://localhost:8080/api/stream/<track>/gapless/alac'
```

Expected: `206` and a size of exactly `end - start + 1`.

- [ ] **Step 7: Commit**

```bash
git add cmd/generate-segments/main.go internal/transcoding/
git commit -m "Add generate-segments backfill command

- builds lossless sets for existing versions with a lossless source
- idempotent: completed sets are skipped, not rebuilt
- one failed version does not stop the run"
```

---

## Done when

- Task 0's harness modes pass on the target device.
- `go test ./...` passes.
- `go build ./...` succeeds.
- `GET /api/media/stream/{id}` without `codecs` returns exactly `{"url": "..."}`.
- `GET /api/media/stream/{id}?codecs=alac,flac` on a lossless-quality track returns a `gapless` block whose fragment ranges match the rows in `track_segment_fragments`.
- A `Range` request against the gapless endpoint returns `206` with exactly the requested bytes.
- A user without access to a track receives `403` from the gapless endpoint.
- `generate-segments` run twice produces no duplicates and no re-encoding.

The client MSE engine is out of scope. It is the second spec.
