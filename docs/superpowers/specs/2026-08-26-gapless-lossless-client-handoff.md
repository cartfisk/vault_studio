# Gapless Lossless Client — Handoff

Date: 2026-08-26
Branch: `claude/gapless-lossless-client`, branched from
`claude/gapless-playback-impl-94536e` (the backend, open as PR #5). Unpushed.

Read this before continuing. It records what is done, what is deliberately not,
and the two things that must happen before any of it can be verified.

## State

Tasks 1-8 of `docs/superpowers/plans/2026-08-26-gapless-lossless-client.md` are
implemented and committed. 104 tests pass, `tsc --noEmit` is clean, and
`MusicPlayer.swapOrder.test.tsx` passes with a zero-line diff across the whole
branch — that test is the guard proving today's A/B playback was moved behind
the interface, not rewritten.

**MP3 playback is unchanged.** That was the overriding constraint throughout.

**Gapless does not happen yet.** The MSE engine is written and tested but is
never instantiated. This is deliberate, not an oversight.

## Why it is not switched on

Two independent reasons, either of which alone would justify stopping.

**Nothing to play.** The backend's `cmd/generate-segments` has never been run
against a real library, so no version has a completed segment set, so the server
returns no `gapless` manifest for any track. The engine would sit idle even if
fully wired, and nothing could be verified end to end.

**Half-wiring is worse than not wiring.** Activating requires four more changes,
and stopping partway means silence on lossless tracks rather than a seam:

1. **`fetchRange` must be injected** into `createMseEngine`. The function exists
   and is tested (`lib/playback/fetchRange.ts`); nothing constructs the engine.
2. **About fifteen timeline-absolute reads remain in `MusicPlayer.tsx`.** Under
   MSE, `element.currentTime` spans the whole queue. Each must go through
   `engine.getTrackTime()`. The same class of bug was already found and fixed in
   `AudioPlayerContext.tsx` — `previousTrack()` read a raw `currentTime` and
   compared it against 3 seconds, which on the third track reads ~600 and
   silently disables the back button.
3. **MediaSession metadata still keys on `currentTrack`,** not the engine's
   `trackchange` event. No `ended` fires at a gapless boundary — that is the
   point of the feature — so the lock screen would show the wrong track.
4. **`MusicPlayer` buffers the element-pair standby for every preload.** That
   must be gated on the preload's engine, or it will load a track the MSE engine
   is about to append.

## Do these in order

1. **Merge or otherwise land the backend** (PR #5).
2. **Run `cmd/generate-segments` against the real library.** Dry-run first. The
   exact commands are in the backend's task-7 report. This is the first time
   that pipeline meets real files, and it is the largest remaining unknown in
   the whole feature — real libraries have formats and edge cases that generated
   fixtures do not.
3. **Then** do the four activation steps above.
4. **Then** the device checks, which nothing automated can substitute for.

## Device checks

None have been run. Do not believe the feature works until they have.

1. An all-lossless album plays with no audible seam.
2. A lossless → MP3 boundary degrades to today's seam, not silence or a stall.
3. The first lossless track after a cold start plays at all. **Most likely
   failure.** iOS refuses to buffer an element that has never played inside a
   user gesture; the unlock is wired on the same gesture as the standby unlock,
   but only hardware will confirm it.
4. The lock screen shows the right track across a gapless boundary.
5. AirPlay via Control Center keeps working across a track transition. System
   routing was measured working with `ManagedMediaSource`; a transition was not.
6. Scrubbing, and waveform comments landing at the right moment. **No automated
   test covers this**, and a leaked offset here is a data-correctness bug that
   presents as a UI glitch.

## Two corrections made during implementation

Both were defects in the spec, found by building against it.

**Timeline offsets were specified in samples.** Summing raw sample counts across
tracks is meaningless when a queue mixes sample rates, which real lossless
libraries do. A 44.1/48/44.1kHz queue placed tracks 81ms and then 177ms wrong,
compounding. It passed three green test runs because every test used one uniform
rate. The reasoning was also wrong: samples were chosen to avoid float drift,
and that drift is 1.5e-11s over 1000 tracks against a 2.27e-5s sample period —
six orders of magnitude below one sample. The precaution cost more than the
problem it prevented. Offsets now sum seconds.

**`PlaybackEngine.load()` required a `GaplessManifest`,** which only exists for
lossless tracks, so the element-pair engine could not satisfy its own interface.
Replaced with `PlayableTrack { trackId, versionId, url, manifest? }`.

## A note on the tests

Three tests on this project passed whether or not the behaviour they named was
present. Each was found by breaking the production code and watching the test
stay green — never by reading it.

If you change anything here, verify the same way. A passing test is evidence
about the test; the evidence about the code is that it fails when the code is
wrong.
