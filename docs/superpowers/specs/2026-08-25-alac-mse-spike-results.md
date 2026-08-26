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

Final device verdicts, measured against 1s + 1s halves (the join arrives a
second after pressing play, which makes A/B comparison practical).

| Mode | Strategy | Device verdict |
|---|---|---|
| AAC, untrimmed | none | click |
| AAC, exact | placement only, no clip | click |
| AAC, trimmed | appendWindow both ends | click |
| AAC, overlap | shift by priming, no clip | click (minimal) |
| AAC, head trim | clip head at frame boundary | click (cleanest of the AAC set) |
| AAC, overlap + edit list | no `+empty_moov` | error — not a valid MSE stream |
| ALAC, trimmed | appendWindow both ends | **no click** |
| ALAC, exact | placement only, no clip | **no click** |
| ALAC, 4min (39MB) | whole-file append | `quota exceeded` |

The comparison that isolates the cause is `ALAC, exact` against `AAC, exact`:
identical strategy, identical placement, and the only difference is that AAC
carries priming and ALAC does not. ALAC is silent; AAC clicks.

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

### 3. Is a sample-accurate join achievable for AAC? — NOT BY CLIENT-SIDE TRIMMING

Every AAC strategy clicks:

- **Untrimmed and exact** leave the 1024-sample priming in the stream, so each
  segment's real audio starts ~23ms late and that silence lands in the join.
- **Windowed** asks Safari to cut mid-frame; Safari clips to the last complete
  frame instead and discards real audio.
- **Overlap** shifts the next segment back by its full 1024-sample priming,
  which overwrites real audio with the incoming segment's silence, because the
  outgoing segment's tail padding is smaller than the shift.
- **Head trim** removes the priming correctly — that cut lands on a frame
  boundary, which Safari accepts — and is audibly the cleanest AAC result. It
  still clicks, because it cannot remove the TAIL padding.
- **Edit list** could not be tested at all: a fragmented MP4 built without
  `+empty_moov` yields `buffered: (none)`. It is not a valid MSE byte stream.

The tail is the wall. AAC packs 44100 real samples plus 1024 priming into 45
frames of 1024 = 46080 samples, leaving 956 samples of padding after the real
audio. The next segment is placed on top of that padding, and MSE's overwrite is
frame-granular in the same way clipping is, so it removes a whole frame of the
outgoing segment along with the padding.

This is the same frame-granularity limit seen at the head, relocated to the
tail, and it is why gapless AAC in practice relies on the container's edit list
rather than on client-side trimming. Our MSE segments strip that edit list,
because `+empty_moov` is what makes them valid MSE segments in the first place.

**The untested mechanism and the chosen delivery mechanism are the same work.**
Approach B is CMAF segmentation, and CMAF tooling emits `init.mp4` plus `.m4s`
media segments with edit lists preserved. Whether Safari honours those edit
lists for priming is the one open question for the lossy tier, and building B
answers it.

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

**The lossless tier is solved.** ALAC joins cleanly with placement alone. No
append windows, no priming compensation, no trim arithmetic in the client — the
backend records true sample counts and the client places segments at running
offsets.

**The lossy tier is unresolved and has three exits.** In preference order:

1. **CMAF with preserved edit lists.** Costs nothing extra, because approach B
   builds CMAF segments regardless. Prove it during B's implementation with the
   harness still in the tree; if Safari honours the edit list, AAC is gapless
   for free.
2. **Ship gapless on lossless only.** ALAC works today. The lossy tier keeps
   today's seam and the player degrades by quality preference rather than
   failing. Costs nothing to build and is the natural fallback if (1) fails.
3. **Serve ALAC to everyone.** Removes the problem by removing AAC, at roughly
   5-10x the bandwidth per stream. Only worth considering if gapless matters
   more than data usage, which for a mobile listener it likely does not.

Do not build a client-side AAC trimming path. Four variants were tested and the
frame-granularity limit defeats all of them; a fifth is unlikely to differ.

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
