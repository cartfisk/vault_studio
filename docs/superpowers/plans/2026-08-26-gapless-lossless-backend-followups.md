# Gapless Lossless Backend — Follow-ups

Date: 2026-08-26
Branch: `claude/gapless-playback-impl-94536e`

Findings raised during implementation and review that were deliberately not
fixed. Each was triaged; none blocks merge. Recorded here because the
per-task ledger they came from is scratch and gets deleted.

## Product decisions, for a human

**Segment sets are excluded from storage accounting.** `internal/db/queries/stats.sql`
sums `track_files.file_size` only. A lossless version now occupies roughly
three times what the report shows (source + ALAC + FLAC). This is
spec-consistent — the design deliberately leaves `track_files` untouched — but
someone should decide knowingly rather than discover it from a full disk.

**No durable record of a failed backfill.** `cmd/generate-segments` builds before
creating rows, so a version with no prior rows whose build fails leaves nothing
in the database. The CLI logs it and counts it; an operator inspecting the
database cannot distinguish "never attempted" from "fails every time." Silence
is the right default — creating rows to record a failure is what caused the
regression fixed in `c89b3a4` — but a `last_attempted_at` column would close it
if repeated failures ever become a support burden.

## Known residual risks

**Partial commit after preflight.** `BuildAllSegmentSets` preflights every
destination before renaming any, then renames. A rename failing in the window
after preflight passed can still leave a partially committed set. Closing it
properly means saving the overwritten bytes for rollback — two-phase commit —
which this does not earn. The window is microseconds and the resulting state
matches what existed before the atomic-commit work.

**Stale rows across an ffmpeg upgrade.** A commit-stage failure can leave a
`completed` row whose file was already swapped. Verified benign today: two
builds of the same source are byte-identical (same MD5, size, and init offset),
so the recorded byte ranges still describe the new file. This would stop being
true if ffmpeg were upgraded between the original build and a repair run.

**No CI coverage of losslessness.** Source, ALAC, and FLAC were confirmed to
decode to byte-identical PCM once, by hand. Nothing on the branch would fail if
the stream-copy path in `BuildSegmentSet` were deleted and every source
re-encoded — `TestBuildSegmentSetFLACStreamCopy` pins the sample count, not the
path taken. A test hashing decoded PCM would close it.

## Defence in depth, not currently exploitable

**`internal/handlers/sharing/track_tokens.go`** resolves `shareToken.VersionID`
without confirming it belongs to `shareToken.TrackID`. Safe today because that
column is written by server code at share-creation time and is never request
input. Worth a check if share creation ever accepts a caller-supplied version.

**404 vs 403 on the gapless routes** lets an authenticated caller distinguish
whether a version id exists globally. Sequential integers behind auth; the fix
costs a query per request.

## Small, mechanical

- `internal/transcoding/mp4frag.go` — the `size == 0` branch makes the bounds
  check vacuously true. Correct per ISO-BMFF (size 0 means "to end of file"),
  but undocumented.
- `internal/transcoding/segments.go` — `sampleCountFor` multiplies before
  checking divisibility; a pathological `duration_ts` could wrap. Unreachable
  for real ffprobe output.
- No test at exactly the 10.000000s fragment-duration boundary. The guard is
  strict-greater by design.
- `TestSegmentCodecsCountMatchesSQLLiteral` catches someone adding a third
  codec, but not someone changing the SQL literal to 3. Making it read the
  `.sql` file would close the reverse direction.
- Access is checked up to three times per gapless request (manifest, signed
  URL, byte serving). Efficiency only — three independent chances to fail
  closed on a revoked share is not a bad property.

## Unrelated, spawned separately

`internal/db/vault.db` is tracked in git — empty, added in the first commit,
present on `main` — and `.gitignore` has no `*.db` rule. Not the live database
(`data/vault.db` is), so nothing leaks today, but a stray write there would put
credential material in a tracked file.
