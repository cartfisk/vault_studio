# ALAC / AAC in MSE — Feasibility Spike

Date: 2026-08-25
Status: Approved, ready for implementation planning

## Why this exists

The near-gapless work (see `2026-08-25-gapless-playback-design.md`) deliberately
deferred true sample-accurate gapless. Resuming that deferred project means
committing to a Media Source Extensions playback engine and to new backend
transcode tiers.

One assumption underneath the intended design is unverified: that iOS Safari's
`ManagedMediaSource` accepts ALAC. Safari plays ALAC in a plain `<audio>`
element, but its MSE codec support is a much narrower list than its element
support, and nothing in the current codebase exercises it.

If that assumption is wrong, the lossless half of the design is dead and the
spec would have been written around it. This spike settles the question before
any spec is written.

## Decisions already made

Recorded here so the spike's results land against a known target. These come
from design interrogation, not from implementation, and none are re-opened by
the spike except where an exit branch says so.

- **Two new MP4 tiers.** AAC-in-fMP4 for the lossy preference, ALAC-in-fMP4 for
  the lossless preference. The original upload becomes download-only. The
  quality preference is still honoured; both paths are intended to be gapless.
- **Eager batch backfill.** A one-time job transcodes the existing library
  rather than transcoding lazily on first play.
- **Whole-file append (approach A).** One `<audio>` element, `srcObject` set to
  `ManagedMediaSource ?? MediaSource`, both tracks appended into the same
  SourceBuffer so they share one timeline. Pre-segmenting with a manifest is a
  follow-on only if device testing shows memory pressure or eviction actually
  hurts.
- **ALAC preserves the source sample rate.** A rate change between tracks forces
  `changeType()` and reintroduces a seam. That is accepted: it is rare within a
  project. The AAC tier normalizes to 44.1k/stereo and is always gapless.
- **Trim data lives on `track_files`.** A migration adds encoder delay, padding,
  and total sample count; the transcoder computes them and the existing DTO
  carries them to the client.
- **The existing two-element swap stays as the fallback engine.** A failed
  `isTypeSupported` or a SourceBuffer error needs somewhere to land, and that
  path already works.

## Questions the spike answers

Ordered by how much a negative result would change the spec.

1. Does `ManagedMediaSource` on iOS Safari accept ALAC?
   `isTypeSupported('audio/mp4; codecs="alac"')`, compared against
   `'audio/mp4; codecs="mp4a.40.2"'` as a control.
2. Do two files appended into one SourceBuffer actually join — a single
   continuous `buffered` range and no audible click at the join?
3. Does the same join click *without* the priming/padding trim? A clean
   untrimmed join would mean subsystem (c), and the migration it needs, is
   smaller than assumed or unnecessary.
4. Does a full-length ALAC append (roughly 30MB) survive on device, or does
   `ManagedMediaSource` evict it under memory pressure?

## Fixtures

The source is synthesized with `ffmpeg`, not taken from the library. It is
fully reproducible, raises no rights questions, and its exact sample count is
known, which the trim maths needs.

One continuous source, split at a point chosen to make a seam unmistakable
rather than a judgment call: a sine sweep for tonal continuity plus a
bar-aligned drum loop for transient continuity, cut mid-note.

From that single source, `ffmpeg` produces four files: two AAC-in-fMP4 halves
and two ALAC-in-fMP4 halves.

A shell script regenerates all of it. The script lives with the harness; the
generated binaries are gitignored and never committed.

## Harness

One static HTML page. No build step, no bundler, no integration with the app.

It selects `ManagedMediaSource ?? MediaSource`, prints `isTypeSupported` for
both codec strings, and offers three actions:

- AAC join, untrimmed
- AAC join, with `appendWindowStart` / `appendWindowEnd` / `timestampOffset`
- ALAC join

After each action it prints the resulting `sourceBuffer.buffered` ranges.

Harness and generator script live in `frontend/public/mse-spike/`, which the
existing Vite dev server serves as-is. `host: true` is already set, so the page
is reachable from the phone over the LAN.

## Success criteria

Not "sounds fine". For each case, three recorded observations:

- the `isTypeSupported` booleans
- whether `buffered` collapses to a single continuous range
- whether the join is audible

Results are written to
`docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md`. The spike is finished when
every question above has a recorded answer, including the negative ones.

## Exit branches

- **ALAC supported** — the spec proceeds as decided above: two MP4 tiers, both
  gapless.
- **ALAC unsupported** — the lossless half is dead. The spec becomes AAC-only,
  and the open question returns: does the lossless preference keep today's seam,
  or does playback force the AAC tier?
- **Untrimmed AAC joins cleanly** — subsystem (c) and its migration shrink or
  disappear.
- **ALAC appends get evicted** — approach A is not viable for the lossless tier
  and pre-segmenting (approach B) moves from follow-on to prerequisite.

## Not in scope

No backend changes. No database changes. No changes to application code. No
MediaSession, lock-screen, or background-playback testing; no seeking within a
joined buffer; no quota probing. Those belong to the implementation spec, which
this spike exists to inform.

The harness is throwaway and is deleted once results are recorded.

## Known risks

- The spike runs over plain HTTP on the LAN. If any of this proves to be
  secure-context-gated on iOS, it needs a tunnel or a local certificate. This
  surfaces on the first run rather than being pre-solved.
- Device only. None of the throttling or eviction behaviour under test
  reproduces in a desktop browser or the simulator.
