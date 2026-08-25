# Near-Gapless Playback Design

Date: 2026-08-25
Status: Approved, ready for implementation planning

## Problem

Track transitions have an audible gap. When a track ends, `AudioPlayerContext`
sets a new `audioUrl`, and `MusicPlayer` responds by assigning `audio.src`,
calling `audio.load()`, waiting for `loadeddata`/`canplay`, and only then
calling `play()`. That load-and-decode round trip is the gap.

A preload path already exists (`preloadNextTrack`, AudioPlayerContext.tsx:303)
but does not help:

- It fires 500ms after the current track *starts*, not near the end.
- It buffers into a detached `new Audio()`. Buffered data in a detached
  element cannot be transferred to a rendered `<audio>`, so `play()` reuses
  only the URL string and the real element refetches.

Two bugs follow from the timing:

- **Expired signed URLs.** `SIGNED_URL_TTL` is 5m. A URL minted 500ms into a
  10-minute track is dead by the time the track ends.
- **Wasted bandwidth.** The next track is guessed and fetched at track start,
  before queue changes, shuffle toggles, or the user navigating away.

## Scope

In scope: `MusicPlayer` and `AudioPlayerContext`.

Out of scope:

- `SharedTrackPlayer` — single track, no queue, nothing to transition to.
- True sample-accurate gapless. See "Deferred" below.

## Target

Near-gapless: roughly 20-80ms seam, from `ended` dispatch latency plus MP3
encoder padding. Inaudible between ordinary tracks; audible on a continuous
mix. Streaming, MediaSession, lock screen, AirPlay, and background playback
all continue to work unchanged.

## Deferred: true sample-accurate gapless

Considered and deliberately not chosen now.

- **Web Audio** (`decodeAudioData` + `start(when)`) gives exact scheduling but
  iOS suspends `AudioContext` on backgrounding, and with no media element
  playing there is no Now Playing, lock screen, or AirPlay. For a music player
  that regression is worse than the gap. A 10-minute lossless track also
  decodes to roughly 200MB of float32.
- **`ManagedMediaSource`** is the Apple-sanctioned path and keeps one `<audio>`
  element, so background and lock screen survive. It requires AAC in
  fragmented MP4; the lossy tier is currently MP3 320k CBR
  (transcoder.go:171), so it needs a new backend transcode tier plus a
  backfill of the existing library. Safari does not support FLAC or WAV in
  MSE, so the lossless tier could not be gapless either way.

Either path also requires trimming encoder delay and padding
(`appendWindowStart`/`appendWindowEnd` plus `timestampOffset`, reading
LAME/Xing or edit-list values). Skipping that trim is what makes naive
"gapless" implementations click.

This work ships the preload timing fix and the element swap. The AAC/MSE work
is a separate project, specced once the residual seam can be heard.

## Architecture

Responsibilities split so that the decidable logic is pure and testable, and
the untestable browser mechanics are isolated.

### `src/lib/gaplessPreload.ts` (new, pure)

- `shouldStartPreload({ currentTime, duration, leadSeconds })`
- `isPreloadStale({ signedAt, now, ttlSeconds })`
- `chooseTransition({ standbySrc, targetUrl, readyState })` -> `"swap" | "load"`

`PRELOAD_LEAD_SECONDS = 20`.

Pure module in `src/lib/`, matching the existing test pattern
(`optimalShuffle.test.ts`, `feedback.test.ts`). Testable in the default node
environment; no jsdom configuration needed.

### `AudioPlayerContext` — queue and URL policy

Decides which track is next and when to mint a fresh signed URL for it.
Exposes `nextTrackPreload: { trackId, url, signedAt } | null`.

The detached-`Audio` code at lines 303-410 is deleted, along with
`getPreloadedAudio` and `clearPreloadedAudio`.

### `MusicPlayer` — element and buffer mechanics

Renders two `<audio>` elements via `elARef` / `elBRef`. Both carry the same
attributes as today's single element: `preload="auto"`,
`crossOrigin="anonymous"`, `playsInline`. Attribute drift between the two
makes the standby buffer unusable.

`audioRef` remains a stable ref object, repointed to whichever element is
active. Object identity never changes, so
`audioPlayerRef.current = { audio: audioRef }` (MusicPlayer.tsx:525) and every
existing `audioRef.current` call site keep working untouched.

The listener effect (MusicPlayer.tsx:377) gains `activeKey` in its deps so it
re-attaches on swap.

## Data flow

1. `onProgressUpdate` already feeds current time to the context. When
   `duration - currentTime <= 20`, fire once per `(currentTrackId,
   nextTrackId, quality)` combination. See "Invalidation" below.
2. Context mints a fresh signed URL for the next track and publishes
   `nextTrackPreload`. Minting at 20s-remaining keeps the URL well inside the
   5m TTL.
3. `MusicPlayer` sets `standby.src = url; standby.load()`. The browser buffers
   ahead.
4. `ended` fires -> existing `onEnded` -> `nextTrack()` -> `play()` ->
   `setAudioUrl`.
5. The `audioUrl` effect calls `chooseTransition`. On `"swap"`: exchange
   active and standby, `play()` the already-buffered element, tear down the
   old one. On `"load"`: today's `src`/`load()`/`canplay` sequence, unchanged.

## iOS gesture unlock

iOS Safari throttles `preload` on an element that has never been touched by a
user gesture. On the first real user-gesture play, both elements get a muted
`play()`/`pause()` pair. Without this the standby buffers nothing and the
ping-pong buys nothing.

This is the single assumption that could invalidate the approach, and it is
only observable on a real device.

## Edge cases

**Invalidation.** The standby is discarded and re-preloaded when the next
track changes: queue reorder, remove, or add inside the final 20s; shuffle
toggled; quality preference changed. The trigger guard is keyed on
`(currentTrackId, nextTrackId, quality)`, not a bare boolean, so a change
re-arms it instead of latching it off.

**Trigger correctness.** Skip when `duration` is 0 or NaN (pre-
`loadedmetadata`). Fire immediately for tracks shorter than the 20s lead. Fire
on a forward seek into the window. Do not re-fire on a backward seek when the
same next track is already buffered. Skip at end of queue.

**Track loop.** `loop={loopMode === "track"}` uses native looping
(MusicPlayer.tsx:843), so `ended` never fires. Preload skips track-loop
entirely.

**Double audio.** After a swap the outgoing element must be paused and have
`src` cleared before the incoming one plays. Two tracks at once is worse than
a gap, so this is an explicit teardown step, not best-effort cleanup.

**Volume.** Applied to the standby before the swap, or every transition steps
on the user's volume setting.

**Auth.** Preserve the existing `canPreload` check
(`!!shareTokenRef.current || isAuthenticated`). Unauthenticated share pages
must not mint stream URLs.

**Manual Next** uses the same swap path and gets faster for free.

## Error handling

Every failure degrades to the current, working load path. Gapless is an
optimization, never a dependency.

- Standby 403s because the URL aged out (user paused inside the window and
  resumed past the 5m TTL) -> `isPreloadStale` re-mints on resume rather than
  waiting to fail; if it still fails, fall back.
- Standby `error` event -> fall back.
- `readyState < 3` at swap time -> fall back.

## Bandwidth

Strictly improves. Today preload fires 500ms into every track regardless. The
new trigger fires only near the end, and only while playing.

A cellular / data-saver opt-out was considered and cut as YAGNI.

## Testing

Unit tests on `src/lib/gaplessPreload.ts`:

- `shouldStartPreload` — threshold crossing, short track, zero and NaN
  duration, forward seek, backward seek, end of queue, track loop.
- `isPreloadStale` — inside TTL, past TTL, paused and resumed.
- `chooseTransition` — matching buffered standby, mismatched src,
  `readyState` below threshold.

Manual iPhone / Safari 26 checklist, none of which a unit test can observe:

- Gesture unlock lets the standby actually buffer.
- Seam is audible or not.
- Lock screen and Now Playing survive the swap.
- Background playback continues across a transition.
- No double audio on rapid Next presses.
