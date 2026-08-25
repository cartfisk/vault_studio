# Gapless Playback — Known Gaps and Follow-ups

Triaged by the final whole-branch review and deliberately shipped as-is. None
block merge; all are worth knowing before someone edits this code.

## 1. Preload reuse does not compare audio quality

`preloadMatches` in `play()` checks trackId, versionId, and staleness, but not
quality. Change the quality preference while paused inside the preload window
and the next track can play at the old quality.

Narrow: on resume the preload effect re-runs and re-mints (its dep array and key
tuple both include `qualityPreference`), so this only lands if the track ends
before that mint resolves. Self-corrects on the next track.

Sitting underneath it is an older, unrelated bug: `play()`'s own dependency array
omits `qualityPreference`, so the `quality` it passes to `getStreamUrl` is
already closure-stale on the non-preload path. That predates this branch. Fixing
the preload tuple without fixing the closure treats the smaller half.

## 2. Unlock retry can be suppressed once a preload lands

`unlockStandbyElement` returns early when the standby already holds a `src`, so
it does not disturb a preload. If the very first unlock attempt fails, later
gestures will mostly find the standby occupied and the retry never happens.

Only reachable if `play()` on a muted 10ms silent data URI inside a real user
gesture fails, which is close to never. The naive fix is hazardous: playing and
pausing a preloaded standby advances its `currentTime`, so the swapped-in track
would start mid-stream unless `currentTime` is reset.

## 3. Waveform warm-up fires in share-token mode

The preload path calls `void ensureTrackWaveform(next)` before the auth gate, so
in share-token mode it hits authenticated endpoints and fails. Equivalent to
today's behaviour — `play()` already calls it unconditionally for the same track
— just ~20s earlier. Costs one failing request.

## 4. Project-loop wrap-around is never gapless

`getNextTrack` returns `null` on the last track of a project, so the
`loopMode === "project"` wrap back to the first track gets no preload and keeps
its gap. Consistent with the spec, which scoped preload to a known next track.

## 5. The standby has no error listener

A preload that 403s or fails does so silently; it surfaces only as an
unexplained fallback to the load path. `chooseTransition` handles it correctly
(a failed element stays at `readyState` 0), but a single `error` listener
logging the failure would make Gate 1 of the device checklist diagnosable.

## 6. Test coverage is narrow by design

`gaplessPreload.test.ts` covers the pure decision logic; one jsdom test pins the
swap's teardown-before-play ordering. Everything else about the feature —
whether the standby buffers on iOS, the actual seam length, lock-screen
behaviour across a swap — is verified only by the device checklist.

## 7. Misleading commit message in branch history

Commit `c1b0f49` is titled "Unlock both audio elements on first play gesture"
but is the fix that changed the behaviour to unlock the standby ONLY. It shares
a subject with `16c6487`, which it reverses. Reword during squash, or correct it
in the merge commit body.
