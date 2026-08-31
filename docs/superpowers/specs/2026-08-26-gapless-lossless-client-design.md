# Gapless Lossless — Client Design

Date: 2026-08-26
Status: approved, not implemented

Second of two specs. The first covered the backend and shipped in PR #5. This
covers the browser playback engine that consumes it.

## Inputs

- `docs/superpowers/specs/2026-08-25-gapless-lossless-backend-design.md`
- `docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md`
- `docs/superpowers/specs/2026-08-25-gapless-lossless-handoff.md`

`docs/superpowers/specs/2026-08-25-gapless-playback-design.md` predates all of
these and is wrong in two places. Kept for history. Do not build from it.

## What the backend provides

`GET /api/media/stream/{trackId}?version_id=&codecs=alac,flac` returns the
existing `{url}` and, when the resolved quality is `lossless` and a completed
set exists for a requested codec, a `gapless` object: `codec`, `url` (carrying
its own `version_id`), `sampleRate`, `sampleCount`, `channels`, `initByteEnd`,
and inclusive `fragments` byte ranges.

`GET /api/stream/{id}/gapless/{codec}?version_id=` serves those bytes by HTTP
Range, behind normal auth.

Omitting `codecs` returns a response byte-identical to the pre-feature one.
That guarantee is what lets this land independently.

## Settled before this spec

ALAC on Safari, FLAC on Chrome and Firefox, chosen by `isTypeSupported` and
never by user agent. MP3 keeps today's seam by design. No AAC, no Web Audio, no
client-side trim arithmetic — ALAC and FLAC carry no encoder priming, so there
is nothing to trim. Each was tried and failed; the results document records how.

## Measured during this design

**System-level AirPlay works during ManagedMediaSource playback.** Verified on
the target iPhone: audio routed from Control Center to an AirPlay device
continues playing.

This closes a gap in the earlier reasoning. `ManagedMediaSource` refuses to
attach unless `disableRemotePlayback = true`, and the spike rejected Web Audio
partly for costing AirPlay — but never checked whether MSE cost it too. It does
not: the flag disables the element's own route picker, not OS output routing.
The app exposes no AirPlay picker of its own, so nothing user-visible is lost.

## Constraint

Every part of the UI must behave exactly as it does today, showing time relative
to the current track only. The MSE timeline spans multiple tracks, so this is
not automatic — it is the requirement the architecture exists to satisfy.

## Architecture

One interface, two implementations. The queue-relative offset never escapes an
engine.

```ts
interface PlaybackEngine {
  load(track: QueuedTrack): Promise<void>
  play(): Promise<void>
  pause(): void
  seekToTrackTime(seconds: number): void
  getTrackTime(): number
  getTrackDuration(): number
  setVolume(v: number): void
  canAppend(next: QueuedTrack): boolean
  prepareNext(next: QueuedTrack): Promise<void>
  teardown(): void
  subscribe(events): Unsubscribe
}
```

`getTrackTime` and `seekToTrackTime` are the whole point. `MseEngine` applies
`trackStartOffset` internally; `ElementPairEngine` passes through. Nothing
outside an engine holds the element, so nothing outside can observe queue time.

`ElementPairEngine` is today's A/B ping-pong moved behind the interface, not
rewritten. Its `canAppend` always returns false. `MusicPlayer.swapOrder.test.tsx`
must keep passing unmodified — that is the guard on "moved, not rewritten."

`MseEngine` owns one `<audio>` with `ManagedMediaSource ?? MediaSource` attached
via `srcObject`, `disableRemotePlayback = true`.

### Files

| File | Owns |
|---|---|
| `lib/playback/types.ts` | Interface and shared types |
| `lib/playback/elementPairEngine.ts` | Existing behaviour, moved |
| `lib/playback/mseEngine.ts` | MediaSource lifecycle, append, evict |
| `lib/playback/timeline.ts` | Offset arithmetic. Pure. |
| `lib/playback/codecSupport.ts` | `isTypeSupported` probe |
| `lib/playback/selectEngine.ts` | Engine choice per track. Pure. |

The three pure modules are testable in Vitest's node environment alongside the
existing `gaplessPreload.ts`. Only the engines need a DOM.

`MusicPlayer.tsx` (1581 lines) loses transport and swap to the engines and keeps
UI and wiring. `AudioPlayerContext` (1101 lines) stops reaching through
`audioPlayerRef.current.audio.current` — fifteen call sites become engine calls.

### Engine selection

Per track, decided when the preload trigger fires. If the current engine can
append the next track, it does and there is no seam. Otherwise the outgoing
engine tears down and the next track's engine loads it, costing today's seam.

An all-lossless album is fully gapless. A mixed album is gapless within its
lossless runs. An all-MP3 album is exactly what it is today.

## Inside MseEngine

**Timeline.** One `SourceBuffer`, `mode = 'segments'`. Each track is appended at
a `timestampOffset` equal to the running sum of the true durations before it.

Each track's own duration is computed exactly at its own rate,
`sampleCount / sampleRate`. Those per-track seconds are then summed.

Two mistakes this avoids, both of which this project made:

- `duration * index` puts every later track in the wrong place, because track
  durations vary. The offset must be a running SUM.
- Summing raw sample COUNTS across tracks is meaningless when a queue mixes
  sample rates, which a real lossless library does routinely. An earlier
  revision of this spec mandated integer samples to avoid float drift; that
  produced offsets 81ms and then 177ms wrong across a 44.1kHz/48kHz/44.1kHz
  queue, compounding down the queue.

Float seconds are safe here and the drift concern that motivated samples was
false: summing 1000 track durations accumulates about 1.5e-11s of error against
a 2.27e-5s sample period, six orders of magnitude below one sample.

Never a float duration read off the element — that is a different number.

**Appending.** Fetch the manifest, append `bytes=0-initByteEnd` once, then
fragments in order by Range through the existing API client, so auth works on
web and in the Capacitor build without signed URLs. Fragment ranges are used
verbatim; the client does no byte arithmetic.

**Backpressure.** Append lazily: while `buffered.end - currentTime >
LEAD_SECONDS`, wait on `timeupdate`. Evict more than `LEAD_SECONDS` behind the
playhead. 30 seconds sits well inside Safari's roughly 15MB audio quota — about
two minutes of ALAC.

Appending faster than playback is what exhausted that quota during the spike.
The harness measured it: 39,268,591 bytes appended progressively with eviction,
no `QuotaExceededError`, `buffered end` exactly 240.0. A buffer-management test
that appends faster than it plays is not testing buffer management.

**Track transitions.** No `ended` fires at a boundary. The engine watches
`timeupdate`, emits `trackchange` when `currentTime` crosses the next offset,
and advances `current`.

**Seeking backward past an eviction.** Re-append from the fragment covering the
target. The manifest's byte ranges make that a lookup.

**Skip.** Remove everything after the current position, reset the timeline, load
fresh. A seam is acceptable: the user asked for a discontinuity.

**Loop.** The `loop` attribute would loop the timeline. Loop mode seeks back to
`current.offset`; `loop` is never set on the MSE element.

**Duration.** `MediaSource.duration` is the timeline's. `getTrackDuration()`
returns the manifest's. Anything reading `element.duration` gets a number that
grows as tracks append.

**Failure.** A fetch error, `QuotaExceededError`, or `SourceBuffer` error tears
the engine down and falls back to `ElementPairEngine` for that track. Playback
continues; the feature degrades. This is a tested path, not a logging `catch`.

## Integration

**Three elements.** The existing A/B pair plus one for MSE. Sharing an element
would mean swapping `srcObject` and `src` on every engine change. Only the MSE
element sets `disableRemotePlayback`.

**iOS unlock.** iOS will not buffer an element that has never played inside a
user gesture. The existing routine unlocks only the standby, deliberately — the
active element is unlocked by the real playback the same gesture starts, and
touching it races that playback. The MSE element needs the same treatment: on
that gesture, attach the MediaSource and play muted briefly. Attaching
`srcObject` makes `play()` resolvable, so no silent data URI is needed, but the
gesture still is.

Unlocking lazily at the first lossless track instead will fail: iOS refuses to
buffer, and the track silently does not play. **This is the most likely failure
in this spec and it cannot be caught by a unit test.**

**MediaSession** is unchanged — metadata lives on `navigator.mediaSession`. The
engine's `trackchange` drives lock-screen updates, since `ended` does not fire.
`setPositionState` takes track-relative values from the engine.

**Preload timing is reused.** `shouldStartPreload` keeps deciding when. What
changes is what preparing means: append into the live timeline, or load a
standby element.

**Teardown** must stop the append loop, not just pause. A loop awaiting
`timeupdate` on a paused element waits forever and holds the MediaSource alive.

## Verification

Every test is checked by breaking the behaviour it covers and confirming it
fails. On the backend half this caught two tests that passed either way.

**Pure, node environment:**

| Unit | Pins |
|---|---|
| `timeline.ts` | Running-sum offsets in seconds; boundary attribution; a mixed sample-rate queue placed correctly; a float-tolerant end-of-track guard |
| `selectEngine.ts` | Lossless + supported codec + completed set → MSE; anything missing → element pair; client codec order decides |
| `codecSupport.ts` | Built from `isTypeSupported`, never user agent; neither supported → no `codecs` param at all |

**Engines, jsdom with fakes:**

- Backpressure: with a fake element whose `currentTime` never advances, appends
  stop. This is the regression that reintroduces the quota failure.
- Eviction fires as the playhead advances and never removes ahead of it.
- Teardown stops a loop awaiting `timeupdate`.
- A mid-track fetch failure falls back and playback continues.
- `MusicPlayer.swapOrder.test.tsx` passes unmodified.

**Device, and only device:**

1. An all-lossless album has no audible seam.
2. A lossless → MP3 boundary degrades to today's seam, not silence or a stall.
3. The first lossless track after a cold start plays — the iOS unlock case.
4. Lock screen shows the right track at the boundary.
5. AirPlay via Control Center keeps working across a track transition.
6. Scrubbing, and waveform comments landing at the right moment.

Item 6 is the one no automated test here covers, and a leaked offset there is a
data-correctness bug that looks like a UI quirk.

## Non-goals

- No gapless indicator in the UI. Gapless availability is invisible to the user
  and depends on whether a version has completed segment sets, so the same album
  may be gapless before a backfill and not after, with nothing explaining why.
  Deliberately deferred, not overlooked.
- No change to how quality preference is chosen.
- No `SharedTrackPlayer` support. Shared links keep today's behaviour.
- No client-side trimming, ever.

## Assumption carried from the backend

The backend was verified against generated fixtures. `cmd/generate-segments` has
not been run against a real library, so real-world formats and edge cases are
unproven. A backend surprise would surface after client work is underway.
