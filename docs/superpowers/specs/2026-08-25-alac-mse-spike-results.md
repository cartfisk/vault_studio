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

## Final codec matrix and decision

Measured on the devices named above.

| Codec in fMP4 | Safari 26.6 (iOS) | Chrome 151 | Join with placement only |
|---|---|---|---|
| ALAC | supported | unsupported | gapless on Safari |
| FLAC | unsupported | supported | gapless on Chrome |
| AAC | supported | supported | clicks everywhere |

ALAC and FLAC are an exact mirror image: each engine supports one and rejects
the other. No single lossless codec covers both.

Both are gapless for the same structural reason. Neither is a transform codec,
so neither carries encoder priming, so nothing ever needs trimming and the
frame-granularity limit that defeated AAC never applies. Both report an
overshoot of exactly 0 samples.

**Decided:**

- **Lossy playback stays MP3, with today's seam.** AAC is abandoned. Gapless is
  a lossless-mode feature.
- **Lossless playback is gapless**, using ALAC on Safari and FLAC on Chrome,
  selected by codec support rather than by user agent.

This removes a large amount of previously planned work: no AAC transcode tier,
no encoder-priming or padding columns, no trim arithmetic in the client, and no
CMAF edit-list dependency. The client places segments at running offsets and
does nothing else.

**Packaging, decided: prefer stream copy.** FLAC into fragmented MP4 is a
remux (`-c:a copy`), verified, not a re-encode — a FLAC upload reaches the
FLAC tier by repackaging alone, at negligible CPU cost and with the samples
untouched. Encode only where the source format leaves no choice: a WAV upload
must be encoded to FLAC, and ALAC always requires a real encode unless the
source is already ALAC. Every such encode is lossless, so no generation loss
accumulates regardless of path.

Open, and deliberately left to the implementation spec: whether both tiers are
stored per version, or one is stored and the other derived on demand and cached.
"Prefer stream copy" settles how each tier is produced, not how many are kept.
Storing both roughly doubles lossless storage per version; deriving on demand
trades that for CPU and a cold-start path. Decide it with real library size
numbers rather than in the abstract.

**Firefox: verified working with FLAC.** FLAC therefore covers Chrome and
Firefox; ALAC covers Safari on both iOS and desktop. Between them the three
engines are served.

**Browser scope, decided:** Safari, Chrome and Firefox are the supported set.
Anything else falls back to the MP3 tier, which means a browser supporting
neither lossless codec plays lossy audio regardless of the user's quality
preference. That is accepted behaviour, not a bug to design around.

**Codec selection is by capability, never by user agent.** Ask
`isTypeSupported` for ALAC, then FLAC, then fall back to MP3. A user-agent
string would break the moment any engine gains support for the other codec.

**Unchanged by this decision:** the quota failure still applies. A whole-file
lossless append is refused on iOS at 39MB, so the lossless tiers still require
pre-segmented delivery.

## Evaluated and rejected: Gapless-5

The [Gapless-5](https://github.com/regosen/Gapless-5) library was tried off the
shelf, playing the same 1s + 1s split in MP3, FLAC and WAV.

Rejected on the same grounds the original design gave for Web Audio generally:
it costs AirPlay and background playback on iOS Safari. This is not a subtlety
to be engineered around — the library's own README states that `useWebAudio`
must be `false` for audio to continue in the background on iOS, and
`useWebAudio` is the setting that provides the gapless. The two requirements are
the same switch in opposite positions. No MediaSession or lock-screen support is
documented.

For a music player, losing the lock screen and AirPlay is a worse regression
than a seam between tracks.

Recorded for accuracy: in this test every format seamed, including WAV. WAV has
no codec and no priming, so a clean WAV join should be automatic; its failure
means the library was not achieving gapless playback in this configuration at
all, and the MP3 and FLAC results here are therefore not a fair assessment of
the library. The rejection does not rest on those results.

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

**The lossy tier keeps its seam, by decision.** MP3 remains the lossy codec and
gapless is a lossless-mode feature. AAC is abandoned rather than pursued through
CMAF: five client-side strategies were tested and the frame-granularity limit
defeats all of them, and the remaining mechanism would have made the lossy tier
the most complicated part of the system to serve the case where quality matters
least.

Do not build a client-side AAC trimming path, and do not revisit AAC without a
reason that outweighs a working MP3 tier.

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
