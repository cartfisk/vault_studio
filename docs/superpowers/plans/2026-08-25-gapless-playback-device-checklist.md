# Gapless Playback — iOS Device Checklist

Device: iPhone, Safari 26 or later. Not simulated — the throttling behavior
under test does not reproduce in a desktop browser or a simulator.

Serve the app on the LAN (`vite.config.ts` already sets `host: true`) and open
it from the phone.

## Gate 1 — the standby actually buffers

- [ ] Start a track longer than 40s from a queue of at least two tracks.
      Start it **by tapping the track row, not the mini-player play
      button** — that is the path that skips `handlePlayPause`, and it is
      the one the gesture unlock has to cover.
- [ ] With Safari Web Inspector attached, watch the Network tab at ~20s
      remaining. A request for the next track's stream URL must appear.
- [ ] The request must transfer a meaningful number of bytes, not just
      respond to a range probe. If it stalls at a few KB, the gesture unlock
      is not working and the rest of this checklist will fail.

## Gate 2 — the seam

- [ ] Let the track run to its end without touching the device.
- [ ] The next track must begin with no visible reload and no more than a
      brief seam. Anything approaching a second means the swap fell back to
      the load path. "Visible reload" means the progress bar or the
      duration readout resetting to 0:00 before the next track begins.
- [ ] Repeat across at least three consecutive transitions, covering three
      DISTINCT tracks — not one track played three times. Repeating a
      single track can mask preload-key latching bugs.

## Gate 3 — no double audio

- [ ] Press Next rapidly five times.
- [ ] Exactly one track must be audible at any moment. Two overlapping tracks
      is a hard failure — the swap teardown ordering is wrong.

## Gate 4 — background and lock screen

- [ ] Start playback, lock the phone.
- [ ] Playback must continue across a track transition while locked.
- [ ] The lock screen Now Playing must update to the new track's title,
      artist, and artwork after the transition.
- [ ] Lock screen play/pause/next must still control playback after a swap.

## Gate 5 — invalidation

- [ ] Inside the last 20s of a track, reorder the queue so a different track
      is next. The newly-chosen track must play next, not the originally
      preloaded one.
- [ ] Inside the last 20s, toggle shuffle. The transition must respect the
      new order.
- [ ] Pause inside the last 20s, wait six minutes, resume, and let the track
      end. The transition must succeed — this exercises the stale signed-URL
      re-mint.
- [ ] Inside the last 20s, change the audio quality preference, then resume and let the track end. The next track should play at the NEW quality. A known gap exists here: the preload match does not compare quality, so it may play at the old quality. Record what you observe.

## Gate 6 — degradation

- [ ] Enable Network Link Conditioner with a slow profile. Transitions must
      still work, falling back to the load path rather than failing or
      producing silence.

## Gate 7 — first-play and volume regressions

These cover defects found during implementation. They are not iOS-specific — check them on desktop too.

- [ ] Load the app fresh, press play on the first track. It must start. (An earlier build stripped the track's own source during the audio-element unlock and the first track never played.)
- [ ] While a track is playing, drag the volume slider through its full range. Playback must continue from the same position and must not restart from 0:00.
- [ ] While a track is playing, press pause and then play. Playback must resume from where it stopped, not restart.
