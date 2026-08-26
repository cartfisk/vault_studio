# ALAC / AAC in MSE — Spike Results

Date: 2026-08-25
Device: iPhone, iOS 18.7, Safari 26.6 (`Version/26.6 Mobile/15E148`)
Served over: plain HTTP on the LAN. No secure-context problem surfaced.
Control: desktop Chrome 151, same harness.

## Capability probe

| Check | iOS Safari 26.6 | Chrome 151 |
|---|---|---|
| ManagedMediaSource present | true | false |
| MediaSource present | false | true |
| AAC supported | true | true |
| ALAC supported | **true** | false |

iOS exposes `ManagedMediaSource` only — plain `MediaSource` is absent, so the
`ManagedMediaSource ?? MediaSource` selection and the `srcObject` attachment
path are both load-bearing rather than defensive.

## Joins

True content is 10.000000s per half (441000 samples @ 44.1kHz), 20.0 joined.

| Mode | Range count | Final buffered end | Join audible? |
|---|---|---|---|
| AAC, untrimmed | 1 | 20.0775 | **click** |
| AAC, trimmed | 1 | 19.9846 | **click** |
| ALAC, trimmed | 1 | 20.0000 | **click** |
| ALAC, 4min (39MB) | — | — | `ERROR: The quota has been exceeded.` |

Per-append detail for the trimmed runs:

| Mode | After append 1 | After append 2 |
|---|---|---|
| AAC, trimmed | 9.9846 | 19.9846 |
| ALAC, trimmed | 9.9381 | 20.0000 |

## Answers

### 1. Does ManagedMediaSource accept ALAC? — YES

`isTypeSupported('audio/mp4; codecs="alac"')` returns true on iOS Safari 26.6.
The lossless half of the two-tier design is not blocked at the codec level.

Chrome returns false, which is expected for an Apple codec and irrelevant to
the decision.

### 2. Do two appends join into one buffered range? — YES, but see 3

Every mode reported `range count: 1`. Both halves land on one continuous
presentation timeline.

This metric turned out to be worthless as a quality signal. Trimmed and
untrimmed both report a single range; so does a join with 77.5ms of garbage in
it. Range count says the timeline is contiguous, not that the audio is correct.

### 3. Does the untrimmed join click? — YES. So does the TRIMMED join.

This is the spike's central result and it contradicts the design's premise.

Untrimmed clicks for the expected reason: the container overshoots the true
content, so 77.5ms of encoder priming and padding is inserted between the
halves (buffered end 20.0775 against a true 20.0).

Trimmed also clicks, for the opposite reason — `appendWindowEnd` on Safari
clips to the last COMPLETE frame rather than to the requested sample:

| | Requested | Delivered | Frame maths | Real audio lost |
|---|---|---|---|---|
| AAC | 10.000000 (441000) | 9.9846 (440320) | 430 x 1024 exactly | 680 samples, 15.4ms |
| ALAC | 10.000000 (441000) | 9.9381 (438272) | 107 x 4096 exactly | 2728 samples, 61.9ms |

Both land on an exact whole-frame count. Desktop Chrome delivered 10.0000 for
the same input and the same window, so this is Safari-specific behaviour, not
an error in the arithmetic.

ALAC is what makes this conclusive. It is lossless, so a correct join would be
bit-exact and silent. It clicked. Audio is genuinely missing, not merely
re-encoded.

**`appendWindow` clipping cannot produce a sample-accurate join on iOS.** The
mechanism the trim design rests on does not work on the target platform.

One unexplained observation: ALAC's second append reported a final buffered end
of exactly 20.0000, where frame-boundary truncation predicts 19.9381. Whether
`buffered` coalesces across the append or the second append genuinely filled to
the boundary is not established. It does not change the conclusion — the join
was audibly broken either way.

### 4. Does a full-length ALAC append survive? — NO

A 39MB, 4-minute ALAC file fails outright:
`ERROR: The quota has been exceeded.`

Whole-file append is not viable for the lossless tier on iOS.

Not tested: a full-length AAC append (~8MB for 4 minutes at 256k). The decision
below made the test moot, but the AAC quota ceiling remains unmeasured.

## Consequences for the implementation spec

Two of the spike design's exit branches fired at once, and they are independent
problems with independent fixes.

**Approach A is dead — both tiers.** Decided after this run. The quota failure
kills whole-file append for ALAC outright, and rather than measure AAC's
ceiling separately and maintain two delivery mechanisms, both tiers move to
pre-segmented delivery (approach B: `init.mp4` plus numbered media segments and
a manifest). This promotes B from a follow-on to a prerequisite and pulls its
costs — a manifest format, many more files per version, per-segment signed-URL
auth — into the main implementation spec.

**The seam is still unsolved, and B does not solve it.** Segmenting changes how
bytes are delivered. It does nothing about frame-granular clipping, which is
what makes the joins click. An implementation spec written on the assumption
that `appendWindow` trims accurately would produce a clicking player no matter
how the segments arrive.

Two untried mechanisms remain, both cheap to test with this same harness:

1. **Overlap-append.** Rather than clipping track N's tail, append track N+1
   with a `timestampOffset` that places its priming samples over track N's
   padding, and let MSE's overlap semantics discard the duplicates. This never
   asks the browser to cut mid-frame, and it is how production MSE players
   handle encoder delay.
2. **Proper CMAF/DASH segmentation.** `+empty_moov` strips the edit list, which
   is why the true sample counts had to be carried out-of-band in
   `fixtures.json` at all. Segmenting with CMAF-aware tooling preserves edit
   lists and correct `baseMediaDecodeTime`, which Safari may honour for priming
   even where `appendWindow` does not.

Neither has been tested. Until one of them produces a silent join on a device,
true sample-accurate gapless is unproven on iOS and the implementation spec
should not assume it.

**Carried forward regardless of mechanism:**

- Container duration is not a usable source of true content length. Two files
  holding identical 441000-sample content produced container durations of
  10.031020 and 10.000000 (ALAC), 10.054240 and 10.023220 (AAC). The backend
  must record true sample counts at transcode time.
- `ManagedMediaSource` with `srcObject` and `disableRemotePlayback = true` is
  the only attachment path on iOS. Plain `MediaSource` does not exist there.
- Trim offsets cannot be `trueDuration * index`. With variable track durations
  the timeline position is a running sum of true durations.

## Recommendation

Do not write the implementation spec yet. Run a second spike round against the
two untried trim mechanisms first. The delivery decision (approach B) is
settled and is not what is blocking; the seam is, and it is the entire point of
the project.

Keep the harness rather than deleting it — round two is a modification of it,
not a rewrite.
