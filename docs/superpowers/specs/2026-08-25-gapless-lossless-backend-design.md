# Gapless Lossless — Backend Design

Date: 2026-08-25
Status: approved, not implemented

This is the first of two specs. It covers everything the server does: producing
lossless fragmented-MP4 renditions, recording their fragment layout, and
exposing both to a client. It stops at the API boundary.

The second spec covers the client MSE playback engine and is deliberately
separate — the manifest is a real seam, and this half can be built and verified
against the existing `frontend/public/mse-spike` harness before any player code
changes.

## Inputs

Read these first. Do not re-derive their conclusions from the code; most were
established by measurement on physical hardware.

- `docs/superpowers/specs/2026-08-25-gapless-lossless-handoff.md`
- `docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md`

`docs/superpowers/specs/2026-08-25-gapless-playback-design.md` predates both and
is wrong in two places. Kept for history. Do not build from it.

## Settled before this spec

| Concern | Decision |
|---|---|
| Lossy playback | MP3, keeps today's ~20-80ms seam. Not gapless. |
| Lossless playback | Gapless. ALAC on Safari, FLAC on Chrome and Firefox. |
| Codec selection | `isTypeSupported`, client-side, never by user agent. |
| Playback engine | MSE via `ManagedMediaSource ?? MediaSource`, one `<audio>`. |
| Client trimming | None. ALAC and FLAC carry no encoder priming. |
| Storage | Store both ALAC and FLAC. Library is small. |
| Placement | `mode='segments'`, `timestampOffset` = running sum of true durations. |

Do not re-attempt AAC in any form, Web Audio or Gapless-5, client-side trim
arithmetic, or a single lossless codec for all browsers. Each was tried and
failed; the results document records how.

## Discovered while designing

Three findings changed the shape of the work relative to the handoff.

**Nothing creates a `lossless` track file today.** Only `source`
(`internal/handlers/tracks/upload.go`) and `lossy` (the transcoder) are ever
written. `lossless` exists in the `CHECK` constraint, the preferences validator,
the stats query, and the fallback chain in `findTrackFile`, but has no writer.
Users who select it silently fall through to source or lossy. The lossless tier
is therefore being built for the first time, not migrated.

**Segments need no signed URLs.** Signed URLs exist because `<audio src>` cannot
carry an auth header. MSE fetches through our own `fetch()`, and
`frontend/src/api/client.ts` already sends `credentials: 'include'` plus
`getAuthorizationHeader()` — cookie on web, bearer in the Capacitor build. The
per-segment signing problem the handoff carried forward does not exist. The MP3
tier keeps signed URLs, unchanged.

**Per-track sample count is sufficient.** Within a track, fragments carry their
own `baseMediaDecodeTime` and self-place. Only the per-track `timestampOffset`
needs a recorded number. There are no per-fragment sample counts.

## Approach

### Storage layout

Two new files per version, beside the existing `lossy.mp3`:

```
<version dir>/
  <source file>          unchanged
  lossy.mp3              unchanged
  gapless-alac.mp4       new
  gapless-flac.mp4       new
```

Each is a single fragmented MP4: `ftyp` + `moov` (the init segment), then
`moof`/`mdat` pairs. Not a directory of numbered segments — the client fetches
fragments by HTTP `Range` against the one file.

This differs from the handoff, which assumed multi-file segments. Byte ranges
keep the exact encoder invocation the spike proved on hardware, avoid pushing
ALAC through the DASH muxer (DASH does not spec ALAC and may refuse), and
replace hundreds of files per version with two.

### Encoding

The invocation from `frontend/public/mse-spike/make-fixtures.sh`, plus an
explicit fragment duration:

```
ffmpeg -i <source> -c:a alac -frag_duration 10000000 \
  -movflags +frag_keyframe+empty_moov+default_base_moof -f mp4 gapless-alac.mp4
```

FLAC is the same with `-c:a flac`.

**Stream copy where the source allows it.** If the source codec already matches
the target, use `-c:a copy` — a remux, provably sample-identical, no encode.

This is not the `-c copy` trap from the spike. That trap is about *cutting* with
`-ss`/`-t`, which cuts at packet boundaries rather than sample boundaries and
silently corrupts sample-exact work. Remuxing without cutting keeps every frame
and is safe. Do not "fix" this later.

### Fragment offsets are measured, not assumed

After encoding, a Go box-walker reads top-level MP4 box headers (4-byte big-endian
size, 4-byte type) and records where each `moof` begins. Whatever ffmpeg actually
emits for `-frag_duration` on an audio-only stream, the recorded layout is the
real one.

No new dependency. Roughly 60 lines. Must handle the 64-bit `largesize` form
(size field of 1) and must return an error rather than partial results on a
truncated or malformed file.

**Fragment count is an acceptance condition, not an assumption.** ffmpeg's
`+frag_keyframe` and `-frag_duration` interact unpredictably on an audio-only
stream, where every frame is a keyframe. The layout is whatever the walker
finds, but a set whose content exceeds the fragment duration and yields only one
fragment has not been fragmented, and appending it whole is the case already
measured to blow the buffer quota. Such a set is marked `failed`. If this fires,
drop `+frag_keyframe` and re-measure rather than lowering the threshold.

### Sample count, cross-checked

Read from each produced file:

```
ffprobe -v error -select_streams a:0 \
  -show_entries stream=duration_ts,time_base,sample_rate,channels \
  -of json <file>
```

The ALAC and FLAC sets must agree on sample count. If they disagree, both are
marked `failed` and neither is stored.

The check is nearly free and catches exactly the class of defect that produced
the two withdrawn findings in the spike results — a defective fixture that
appeared to show ALAC padding its final frame.

### Lossy sources are skipped

Segment sets are generated only when the source is lossless. Reuse the existing
predicate `isLosslessCodec` in `internal/transcoding/metadata.go` (exported for
this) rather than writing a second one. It already covers flac, alac, ape,
wavpack, tta, and the pcm variants.

An mp3, aac, ogg, opus, or wma upload gets today's behavior exactly: a `source`
row, a `lossy.mp3`, no sets, no `gapless` in the API response, no storage spent.
Encoding a lossy source to ALAC would bake its decoder priming into the samples
as real silence — mechanically gapless, audibly identical to the seam.

Video uploads already pass through `ExtractAudioToWAV` to `pcm_s24le`, so they
read as lossless and do get sets. No special case needed.

### Job pipeline

`transcoding.Job` gains `Kind` (`lossy` | `segments`). `TranscodeVersion` creates
the two set rows as `pending` and queues one `segments` job alongside the
existing MP3 job.

Two jobs, not one and not three. MP3 is the fast path the user hears first and
must not wait on lossless encoding, so it stays separate. Both codecs share a
single job because the sample-count cross-check below needs both results in one
place; splitting them per codec would leave that check with no owner.

**The websocket contract does not change.** Only the lossy job calls
`NotifyTranscodingUpdate`. Segment jobs update their row silently. The client
discovers gapless availability when it requests a stream URL, not through a
status event. This is what lets the frontend spec ship independently of this one.

**Failure is invisible.** A failed segment job sets `status='failed'` and logs.
The stream-url response omits `gapless`. Playback never blocks and never degrades
below what exists today.

## Schema

One migration, `036_add_gapless_segments.sql`. Conventions follow `034`.

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

`init_byte_end` holds the end of the `ftyp`+`moov` prelude. The client appends
bytes `0..init_byte_end` once to initialize the SourceBuffer, then appends
fragments. Keeping it on the set means `track_segment_fragments` has exactly one
meaning and the client's loop never special-cases index 0.

`byte_end` is inclusive, matching HTTP `Range: bytes=start-end`, so no arithmetic
happens between the database and the header.

`sample_count` and `sample_rate` are an integer pair rather than a float
duration, so the client can do exact rational arithmetic and avoid accumulating
float error across a long queue.

`UNIQUE (version_id, codec)` gives the transcoder and the backfill command an
idempotent upsert target.

`track_files` is not touched. No row migration, no change to the `quality`
`CHECK`, no change to the stats query or to `findTrackFile`.

## API

### Extended: `GET /api/media/stream/{trackId}`

New optional parameter `codecs`, comma-separated, in the client's own preference
order, built from its `isTypeSupported` probe.

```
GET /api/media/stream/abc123?version_id=42&codecs=alac,flac
```

```json
{
  "url": "/api/stream/abc123?user_id=1&expires=...&signature=...",
  "gapless": {
    "codec": "alac",
    "url": "/api/stream/abc123/gapless/alac?version_id=42",
    "sampleRate": 44100,
    "sampleCount": 10584000,
    "channels": 2,
    "initByteEnd": 1023,
    "fragments": [
      { "start": 1024, "end": 442367 },
      { "start": 442368, "end": 884201 }
    ]
  }
}
```

`url` keeps its exact current meaning and stays signed. It is the MP3 path and
the fallback.

**Omit `codecs` and the response is byte-identical to today's.** This is the
compatibility guarantee that lets the frontend land separately.

`gapless` is present only when all of:

- `codecs` was sent and non-empty
- `resolveQuality` returns `lossless` — the requested or preferred tier, read
  before `findTrackFile` applies its fallback chain
- a `completed` set exists for one of the requested codecs

Note the asymmetry, because an implementer will hit it: since nothing writes
`lossless` rows to `track_files`, `findTrackFile` will fall back and `url` will
point at the source or lossy file even when `gapless` is present. That is
today's behavior and this spec does not change it. `url` is the fallback for a
client that declines the gapless path; it is not required to be the same tier.

The server intersects and takes the client's first match. Codec preference is
the client's; availability is the server's.

`MediaHandler` currently holds only `auth.Config` and gains a `*db.DB`.

### New: `GET /api/stream/{trackId}/gapless/{codec}?version_id=`

Serves the fragmented MP4 through `http.ServeContent`, which already handles
`Range`, `206`, and malformed ranges. `Content-Type: audio/mp4`. The client
issues one Range request per fragment.

Not signed. Normal `AuthMiddleware`.

**This endpoint must run the same `tracks.CheckTrackAccess` that `StreamTrack`
does, against the same resolved version.** Skipping it makes revoked shares
playable through the gapless path — a silent bypass of the only check enforcing
revocation. This has a required test.

## Backfill

`cmd/generate-segments`, a one-shot command following the existing
`cmd/generate-waveforms` and `cmd/clear-waveforms` pattern. Run manually.

Idempotent via `UNIQUE (version_id, codec)`: re-running skips completed sets and
does not duplicate or re-encode. Doubles as the repair tool when a set fails.

## Verification

### Step zero, before implementation

Byte-range appending is the one unproven assumption in this design. It is
verified first, and if it fails the design changes rather than the schedule.

Add a mode to `frontend/public/mse-spike` that takes `big-alac.mp4` — already
generated by `make-fixtures.sh` as 240s of incompressible pink noise, made
specifically to approximate a real lossless track's buffer footprint — fetches
it in fragment-sized `Range` requests, appends progressively, and evicts behind
the playhead.

Pass condition: plays to completion with no `QuotaExceededError`, and
`buffered.end` matches the true content length. Run on the same physical device
the earlier results came from.

Second mode: a two-track join by byte range, ALAC and FLAC. The spike proved the
join; this proves it survives the delivery change.

Recall from the spike: `range count: 1` does not mean a clean join. A join with
77ms of padding also reports one range. Compare `buffered.end` against the true
content length.

### Go tests

| Test | What it pins |
|---|---|
| Box-walker over a generated fMP4 fixture | Fragment offsets correct, including a final fragment ending at EOF |
| Box-walker over a truncated file | Errors rather than returning bogus offsets |
| Box-walker over a 64-bit `largesize` box | Handles the extended size form |
| Single-fragment output for a long track | Marked `failed`, not stored |
| Sample-count disagreement | Both sets marked `failed`, neither stored |
| Lossy source | No sets created; MP3 job still queued |
| Stream-copy path | FLAC source produces a FLAC set without re-encoding |
| `StreamURL` without `codecs` | Response byte-identical to today's |
| `StreamURL` with `codecs`, no completed set | `gapless` omitted, `url` present |
| `StreamURL` codec intersection | Client order wins when both sets exist |
| Gapless bytes endpoint, revoked access | 403 — the bypass guard |
| Gapless bytes endpoint, Range request | 206 with the requested bytes |

### Out of scope here

Anything requiring MSE in a real browser beyond the two harness modes above.
This spec is verified when the harness passes and a `Range` request via `curl`
returns the right bytes for a real uploaded track.

Backfill is verified by running `cmd/generate-segments` twice over the real
library and confirming no duplicates and no re-encoding.

## The harness

`frontend/public/mse-spike/` is not deleted by this work. It is how a new
browser, codec, or delivery change gets verified without rebuilding anything.

Serve with `npm run dev` from `frontend/` and open
`http://<host>:3000/mse-spike/index.html`. The explicit `index.html` is required;
the bare directory path is swallowed by the TanStack SPA fallback.

Delete it once the client implementation is proven.
