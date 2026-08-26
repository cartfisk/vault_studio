# ALAC / AAC in MSE — Experiment Results

Date: 2026-08-25
Device: iPhone, iOS 18.7, Safari 26.6 (`Version/26.6 Mobile/15E148`)
Served over: plain HTTP on the LAN. No secure-context problem surfaced.
Control: desktop Chrome 151, same harness.

Two rounds were run. Round one used a defective fixture; its conclusions are
superseded and are recorded here only where they still hold.

## The fixture defect

Round one split the 20s source with `ffmpeg -t 10 -c copy`. WAV stream copy cuts
at packet boundaries of 4096 samples, not at the requested sample, so the first
half came out 442368 samples instead of 441000 and **overlapped the second half
by 1368 samples — 31ms of duplicated audio**. No join could have been clean.

Re-encoding the cut with `-c:a pcm_s16le` produces exactly 441000 samples per
half. Every number below is from the corrected fixture unless stated.

Two round-one findings were artifacts of this defect and are **withdrawn**:

- *"ALAC pads its final frame to a 4096-sample boundary."* False. That was the
  corrupt half (442368 = 108 x 4096). With a correct input ALAC reports exactly
  10.000000 and overshoots by zero samples.
- *"Container durations are inconsistent across identical content."* False. Both
  halves now report identical durations. The inconsistency was the defect.

One round-one finding survives: **Safari's `appendWindowEnd` clips to the last
complete frame**, discarding real audio when the requested cut falls mid-frame.

## Capability probe

| Check | iOS Safari 26.6 | Chrome 151 |
|---|---|---|
| ManagedMediaSource present | true | false |
| MediaSource present | false | true |
| AAC supported | true | true |
| ALAC supported | **true** | false |

iOS exposes `ManagedMediaSource` only. The `ManagedMediaSource ?? MediaSource`
selection and the `srcObject` attachment path are both required, not defensive.

## Encoder overshoot, corrected fixture

True content is 441000 samples (10.000000s) per half.

| File | Container duration | Overshoot |
|---|---|---|
| h1-aac / h2-aac | 10.023220 | 1024 samples (priming) |
| h1-alac / h2-alac | 10.000000 | **0 samples** |

ALAC carries no priming and reports its true length. AAC carries exactly one
frame of priming, consistently across both halves.

## Results

Device verdicts are from listening on the iPhone. A 440Hz tone split mid-cycle
makes any discontinuity audible as a click.

| Mode | Strategy | Buffered end (Chrome) | Device verdict |
|---|---|---|---|
| AAC, untrimmed | none | 20.0775 | click |
| AAC, trimmed | appendWindow both ends | 19.9846 | click |
| AAC, overlap | shift by priming, no clip | 20.0000 | click (minimal) |
| AAC, overlap + edit list | as above, no `+empty_moov` | `(none)` | error |
| AAC, head trim | clip head at frame boundary | 20.0000 | *pending* |
| ALAC, trimmed | appendWindow both ends | — | **no click** |
| ALAC, exact | placement only, no clip | — | **no click** |
| ALAC, 4min (39MB) | whole-file append | — | `quota exceeded` |

## Answers

### 1. Does ManagedMediaSource accept ALAC? — YES

The lossless tier is not blocked at the codec level.

### 2. Is a sample-accurate join achievable on iOS? — YES for ALAC

Both ALAC strategies are audibly clean, including the one that does no trimming
whatsoever. The reason is structural: ALAC overshoots by zero samples, so no
append window ever has to cut anything, and Safari's frame-granular clipping
never engages. Placement alone is sufficient.

This is the strongest result of the experiment. **The lossless tier can be
truly gapless with no trim arithmetic anywhere in the client.**

### 3. Is a sample-accurate join achievable for AAC? — NOT YET

Every AAC strategy tested so far clicks:

- **Untrimmed** inserts the 1024-sample priming as audible content.
- **Windowed** asks Safari to cut at sample 441000, which is mid-frame; Safari
  clips to the last complete frame (430 x 1024 = 440320) and discards 680
  samples of real audio.
- **Overlap** shifts the next segment back by its full 1024-sample priming, but
  the previous segment's tail padding is only 344 samples. The incoming
  priming therefore overwrites 680 samples of real audio with silence — a
  smaller defect, heard as a minimal click.
- **Edit list** could not be tested: a fragmented MP4 built without
  `+empty_moov` yields `buffered: (none)` with no error. It is not a valid MSE
  byte stream. Reaching edit lists would require real CMAF/DASH muxing.

**Head trim** is the outstanding candidate: clip the priming off the head rather
than the tail. Priming is exactly one AAC frame, so that cut is frame-aligned
and does not require Safari to cut mid-frame. Result pending.

### 4. Does a full-length ALAC append survive? — NO

A 39MB, 4-minute ALAC file fails with `The quota has been exceeded.` Whole-file
append is not viable for the lossless tier.

A full-length AAC append (~8MB for 4 minutes at 256k) was not tested; the
decision below made it moot.

## Consequences for the implementation spec

**Approach A is dropped for both tiers.** Decided after the quota failure.
Rather than measure AAC's ceiling separately and maintain two delivery
mechanisms, both tiers move to pre-segmented delivery (approach B: `init.mp4`
plus numbered media segments and a manifest). This pulls a manifest format,
many more files per version, and per-segment signed-URL auth into the main
implementation spec.

Note that this decision is about **delivery**, and delivery was never the cause
of the clicks. Segmenting alone would not have produced a gapless player.

**The lossless tier is solved.** ALAC joins cleanly with placement alone.

**The lossy tier is not, pending the head-trim result.** If head trim is clean,
the client needs the priming sample count per file and nothing else. If it
clicks, the remaining options are real CMAF muxing with preserved edit lists,
or accepting a residual seam on the AAC tier only.

**Carried forward regardless of mechanism:**

- The backend must record the true sample count and the encoder priming per
  file. ALAC's priming is zero; AAC's was a consistent 1024 here but must be
  measured rather than assumed, since it is encoder- and version-dependent.
- `ManagedMediaSource` with `srcObject` and `disableRemotePlayback = true` is
  the only attachment path on iOS.
- Timeline offsets cannot be `trueDuration * index`. With variable track
  durations the offset is a running sum of true durations.
- Fixture and transcode pipelines must never cut audio with `-c copy`. It cuts
  at packet boundaries and silently corrupts sample-exact work.
