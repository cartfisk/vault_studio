# Gapless Playback — Handoff

Date: 2026-08-25
Branch: `gapless-lossless-codecs` (unpushed), in the worktree
`.claude/worktrees/alac-mse-spike`
Prior branch in the chain: `worktree-alac-mse-spike`, also unpushed

For a session starting with no context. Read this first, then the results
document it points at. **Do not re-derive any of this from the code** — most of
it was established by measurement on physical hardware, and several conclusions
are the opposite of what the code and earlier documents suggest.

## Where the project is

Near-gapless playback already shipped: two `<audio>` elements ping-pong, the
next track preloads at ~20s remaining, and the swap leaves a ~20-80ms seam.
That is on `main` (PRs #2 and #3). It works.

True sample-accurate gapless was then investigated. That investigation is
**complete**, and its output is a settled design that has not yet been
implemented.

## The design, settled

| Concern | Decision |
|---|---|
| Lossy playback | MP3, keeps today's seam. Not gapless. |
| Lossless playback | Gapless. ALAC on Safari, FLAC on Chrome and Firefox. |
| Codec selection | `isTypeSupported`, ALAC then FLAC, else fall back to MP3. Never by user agent. |
| Playback engine | MSE via `ManagedMediaSource ?? MediaSource` on one `<audio>` element. |
| Client trimming | None. Placement only — segments at running offsets of true durations. |
| Delivery | Pre-segmented. Whole-file append hits a quota ceiling. |
| Packaging | Prefer stream copy; encode only where the source format leaves no choice. |
| Browser scope | Safari, Chrome, Firefox. Anything else gets MP3 regardless of preference. |

Everything above is backed by measurement. The evidence, including the numbers
and the device used, is in
`docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md`.

## What must not be re-attempted

Each of these was tried and failed. Re-trying them costs days and ends the same
way.

- **AAC in any form.** Five client-side strategies were tested (untrimmed,
  exact placement, appendWindow trimming, overlap, head trim). All click.
  Safari's `appendWindow` clips to the last complete frame, so trimming
  discards real audio; the tail padding cannot be removed at all. AAC is
  abandoned, including the CMAF edit-list route, which works but would make the
  lossy tier the most complex part of the system to serve the case where
  quality matters least.
- **Web Audio, including the Gapless-5 library.** Costs AirPlay and background
  playback on iOS Safari. Gapless-5's own docs require `useWebAudio: false` for
  background audio, and that same flag is what provides the gapless.
- **Client-side trim arithmetic of any kind.** ALAC and FLAC carry no encoder
  priming, so there is nothing to trim. Adding trimming would be work that
  makes the result worse.
- **A single lossless codec for all browsers.** ALAC and FLAC are an exact
  mirror image: Safari supports only ALAC, Chrome and Firefox only FLAC.

## Corrections to earlier documents

`docs/superpowers/specs/2026-08-25-gapless-playback-design.md` predates all of
this and is wrong in two places. It is kept for history; do not build from it.

- It says ManagedMediaSource requires AAC in fragmented MP4. ALAC works.
- It assumes a new AAC transcode tier plus a library backfill. Neither is
  needed.

An earlier revision of the results document contained two findings that were
artifacts of a defective test fixture — that ALAC pads its final frame, and that
container durations are inconsistent. Both are withdrawn and marked as such in
the current version. If you see those claims quoted anywhere else, they are
stale.

## Still open

1. **Store both lossless tiers, or store one and derive the other on demand?**
   ALAC and FLAC are inter-convertible without generation loss. Storing both
   roughly doubles lossless storage per version; deriving trades that for CPU
   and a cold-start path. Decide with real library size numbers.
2. **Segmentation format and auth.** Pre-segmented delivery needs a manifest
   format, many more files per version, and per-segment signed URLs. The
   current signing scheme signs one URL per track file.
3. **Backend must record true sample counts.** The client places segments at
   running offsets of true durations, and container duration is not always the
   true content length. Timeline offsets are a running sum, never
   `trueDuration * index` — track durations vary.

## The test harness

`frontend/public/mse-spike/` holds a working MSE test harness: a fixture
generator (`make-fixtures.sh`, ffmpeg, output gitignored) and a page that probes
codec support and runs a two-segment join under nine different strategies.

Serve it with `npm run dev` from `frontend/` and open
`http://<host>:3000/mse-spike/index.html`. **The explicit `index.html` is
required** — the bare directory path is swallowed by the TanStack SPA fallback.

It was deliberately not deleted. It is how you would verify a new browser, a new
codec, or a segmentation change without rebuilding anything. Delete it once the
implementation is proven.

Two traps it encodes, both of which cost real time to find:

- Never cut audio with `ffmpeg -c copy`. It cuts at packet boundaries, not
  sample boundaries, and silently corrupts sample-exact work. Re-encode instead.
- `range count: 1` from a SourceBuffer does not mean a clean join. A join with
  77ms of padding in it also reports one range. Compare the buffered END value
  against the true content length.

## Suggested next step

Brainstorm the implementation design for pre-segmented lossless delivery, then
write a plan, then execute. Treat this document and the results document as the
inputs. The remaining work is smaller than it looked at the start: no AAC tier,
no priming or padding columns, no client trim module, no CMAF dependency.
