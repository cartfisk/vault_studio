# ALAC / AAC in MSE Feasibility Spike — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Determine on a physical iPhone whether ALAC and AAC in fragmented MP4
can be appended into a single MSE SourceBuffer to produce a sample-accurate
gapless join, before any implementation spec for true gapless playback is
written.

**Architecture:** A throwaway static page under `frontend/public/mse-spike/`,
served by the existing Vite dev server, plus a shell script that synthesizes its
fixtures with `ffmpeg`. No application code, no backend, no database. The page
attaches a `ManagedMediaSource` (or `MediaSource`) to one `<audio>` element,
appends two half-tracks into one SourceBuffer, and reports what happened.

**Tech Stack:** `ffmpeg` / `ffprobe` (already installed), plain HTML plus ES
modules, the existing Vite dev server (`host: true` is already set), Vitest for
the one unit-testable piece.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-08-25-alac-mse-spike-design.md`.
- Everything lives under `frontend/public/mse-spike/`. No file outside that
  directory is modified except `.gitignore` and the results document.
- Generated audio binaries are gitignored and never committed.
- No changes to application code, backend, or database. This is a spike.
- The harness is deleted once results are recorded.
- Fixture audio is synthesized, never taken from the library.
- Verification is by recorded observation on a physical iPhone (Safari 26+).
  Desktop runs are a smoke test only — the throttling and eviction behaviour
  under test does not reproduce off-device.

## A note on testing

This plan has almost no unit tests, and that is deliberate rather than an
oversight. The spike's entire deliverable is observed browser behaviour on a
device. A unit test asserting `isTypeSupported` returns true would only assert
whatever the local browser happens to do, which is the thing in question. Task
gates are therefore "run this exact command, confirm this exact output" and, for
the device tasks, "record the observation".

The one genuinely testable piece — the trim arithmetic — is Task 4.

---

### Task 1: Fixture generator

Synthesizes one continuous tone, splits it mid-cycle, and encodes both halves to
AAC-in-fMP4 and ALAC-in-fMP4. A 440Hz sine is the classic gapless test signal:
any discontinuity at the join is an audible click, and any inserted silence is
obvious.

**Files:**
- Create: `frontend/public/mse-spike/make-fixtures.sh`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: nothing.
- Produces: `frontend/public/mse-spike/fixtures/` containing `h1-aac.mp4`,
  `h2-aac.mp4`, `h1-alac.mp4`, `h2-alac.mp4`, and `big-alac.mp4`. Task 2 adds
  `fixtures.json` to the same directory.

- [ ] **Step 1: Ignore the generated binaries**

Append to `.gitignore`:

```gitignore
# Throwaway MSE spike fixtures (regenerate with make-fixtures.sh)
frontend/public/mse-spike/fixtures/
```

- [ ] **Step 2: Write the generator script**

Create `frontend/public/mse-spike/make-fixtures.sh`:

```bash
#!/usr/bin/env bash
# Regenerates the MSE spike fixtures. Output is gitignored.
#
# One 20s 440Hz tone, split at exactly 10.000s (441000 samples @ 44.1kHz).
# The split lands mid-cycle, so a bad join clicks audibly.
set -euo pipefail

cd "$(dirname "$0")"
out="fixtures"
mkdir -p "$out"
rm -f "$out"/*.wav "$out"/*.mp4

frag="+frag_keyframe+empty_moov+default_base_moof"

# Continuous source, then two exact halves.
ffmpeg -v error -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=20" \
  -ac 2 -c:a pcm_s16le "$out/source.wav"
ffmpeg -v error -i "$out/source.wav" -t 10 -c copy "$out/h1.wav"
ffmpeg -v error -ss 10 -i "$out/source.wav" -c copy "$out/h2.wav"

for half in h1 h2; do
  ffmpeg -v error -i "$out/$half.wav" -c:a aac -b:a 256k \
    -movflags "$frag" -f mp4 "$out/$half-aac.mp4"
  ffmpeg -v error -i "$out/$half.wav" -c:a alac \
    -movflags "$frag" -f mp4 "$out/$half-alac.mp4"
done

# A pure tone compresses to almost nothing in ALAC, which would not exercise
# the memory question at all. Noise is incompressible, so this approximates a
# real lossless track's buffer footprint.
ffmpeg -v error -f lavfi -i "anoisesrc=r=44100:d=240:c=pink" -ac 2 \
  -c:a alac -movflags "$frag" -f mp4 "$out/big-alac.mp4"

rm -f "$out"/*.wav
ls -l "$out"
```

- [ ] **Step 3: Make it executable and run it**

```bash
chmod +x frontend/public/mse-spike/make-fixtures.sh
./frontend/public/mse-spike/make-fixtures.sh
```

Expected: five `.mp4` files listed, no `.wav` files remaining. The AAC halves
are roughly 270KB each, the ALAC halves roughly 170KB, and `big-alac.mp4` is
tens of MB.

- [ ] **Step 4: Confirm the containers are what MSE needs**

```bash
ffprobe -v error -show_entries stream=codec_name,sample_rate,channels,duration -of default=nw=1 frontend/public/mse-spike/fixtures/h1-aac.mp4
```

Expected exactly:

```
codec_name=aac
sample_rate=44100
channels=2
duration=10.054240
```

That duration is longer than the 10.000000s source. The excess is the encoder
priming and padding this spike exists to measure.

```bash
ffprobe -v error -show_entries stream=codec_name,duration -of default=nw=1 frontend/public/mse-spike/fixtures/h1-alac.mp4
```

Expected exactly:

```
codec_name=alac
duration=10.031020
```

10.031020s is 442368 samples, which is exactly 108 ALAC frames of 4096. ALAC
adds no head priming but pads its final frame, so it overshoots too. Both
formats need trimming; neither is trimmed yet.

- [ ] **Step 5: Commit**

```bash
git add .gitignore frontend/public/mse-spike/make-fixtures.sh
git commit -m "Add MSE spike fixture generator"
```

---

### Task 2: Record the true sample counts

The trim needs exact sample counts, and the containers do not carry them:
`+empty_moov` drops the edit list, so both files report `start_time=0` with no
priming information. The counts have to come from the known source instead and
be written where the harness can read them.

**Files:**
- Modify: `frontend/public/mse-spike/make-fixtures.sh`

**Interfaces:**
- Consumes: the fixture files from Task 1.
- Produces: `frontend/public/mse-spike/fixtures/fixtures.json`, shape:

```json
{
  "sampleRate": 44100,
  "trueSamplesPerHalf": 441000,
  "trueDurationPerHalf": 10.0,
  "encoded": {
    "h1-aac.mp4":  { "containerDuration": 10.054240 },
    "h2-aac.mp4":  { "containerDuration": 10.054240 },
    "h1-alac.mp4": { "containerDuration": 10.031020 },
    "h2-alac.mp4": { "containerDuration": 10.031020 }
  }
}
```

- [ ] **Step 1: Emit the manifest from the generator**

In `make-fixtures.sh`, insert this immediately before the final
`rm -f "$out"/*.wav` line:

```bash
# The container duration overshoots the true content: AAC by its priming and
# padding, ALAC by padding its last frame out to 4096 samples. The harness
# needs the true count to trim against, and it is not in the file.
{
  echo '{'
  echo '  "sampleRate": 44100,'
  echo '  "trueSamplesPerHalf": 441000,'
  echo '  "trueDurationPerHalf": 10.0,'
  echo '  "encoded": {'
  first=1
  for f in h1-aac h2-aac h1-alac h2-alac; do
    dur=$(ffprobe -v error -show_entries stream=duration \
      -of default=nw=1:nk=1 "$out/$f.mp4")
    [ $first -eq 0 ] && echo ','
    first=0
    printf '    "%s.mp4": { "containerDuration": %s }' "$f" "$dur"
  done
  echo
  echo '  }'
  echo '}'
} > "$out/fixtures.json"
```

- [ ] **Step 2: Regenerate and inspect**

```bash
./frontend/public/mse-spike/make-fixtures.sh
cat frontend/public/mse-spike/fixtures/fixtures.json
```

Expected: valid JSON with four `containerDuration` values, each greater than or
equal to `trueDurationPerHalf` of 10.0.

Do not expect them to be equal to each other, or all to exceed 10.0. Measured on
this machine, two files holding identical 441000-sample content came out as
`h1-alac` 10.031020 (padded to 108 frames of 4096) and `h2-alac` 10.000000
(exact), with `h1-aac` 10.054240 against `h2-aac` 10.023220. That inconsistency
is the finding, not a fault: container duration cannot be trusted as the true
content length, which is precisely why this manifest exists.

- [ ] **Step 3: Confirm it parses**

```bash
python3 -m json.tool frontend/public/mse-spike/fixtures/fixtures.json > /dev/null && echo "valid JSON"
```

Expected: `valid JSON`.

- [ ] **Step 4: Commit**

```bash
git add frontend/public/mse-spike/make-fixtures.sh
git commit -m "Emit true sample counts alongside spike fixtures"
```

---

### Task 3: Capability probe page

The smallest thing that answers question 1. Getting this onto a device early is
worth more than a complete harness later: a `false` here reshapes the entire
design, and everything after this task becomes moot for the ALAC tier.

**Files:**
- Create: `frontend/public/mse-spike/index.html`

**Interfaces:**
- Consumes: nothing.
- Produces: a page at `/mse-spike/index.html` that renders a support table, plus the
  module-scope bindings Task 5 extends in place — `log(line)`, `Impl` (the
  `ManagedMediaSource` or `MediaSource` constructor, or `undefined`), and the
  `AAC` / `ALAC` MIME strings. The matching `window.spike*` globals exist only
  so the same values can be poked at from a Web Inspector console on the
  phone, where there is no other way to inspect them.

- [ ] **Step 1: Write the page**

Create `frontend/public/mse-spike/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>MSE codec spike</title>
  <style>
    body { font: 16px -apple-system, system-ui, sans-serif; margin: 1rem; }
    table { border-collapse: collapse; width: 100%; margin-bottom: 1rem; }
    td, th { border: 1px solid #ccc; padding: .4rem; text-align: left; }
    .yes { color: #0a0; font-weight: 600; }
    .no { color: #c00; font-weight: 600; }
    pre { background: #f4f4f4; padding: .5rem; overflow-x: auto; }
  </style>
</head>
<body>
  <h1>MSE codec spike</h1>
  <table id="support"></table>
  <h2>Log</h2>
  <pre id="log"></pre>

  <script type="module">
    const AAC = 'audio/mp4; codecs="mp4a.40.2"';
    const ALAC = 'audio/mp4; codecs="alac"';

    const logEl = document.getElementById('log');
    function log(line) {
      logEl.textContent += line + '\n';
    }

    log('user agent: ' + navigator.userAgent);

    const hasManaged = 'ManagedMediaSource' in window;
    const hasPlain = 'MediaSource' in window;
    const Impl = window.ManagedMediaSource ?? window.MediaSource;

    const rows = [
      ['ManagedMediaSource present', hasManaged],
      ['MediaSource present', hasPlain],
      ['AAC supported', Impl ? Impl.isTypeSupported(AAC) : false],
      ['ALAC supported', Impl ? Impl.isTypeSupported(ALAC) : false],
    ];

    document.getElementById('support').innerHTML =
      '<tr><th>Check</th><th>Result</th></tr>' +
      rows.map(([label, ok]) =>
        `<tr><td>${label}</td><td class="${ok ? 'yes' : 'no'}">${ok}</td></tr>`
      ).join('');

    for (const [label, ok] of rows) log(`${label}: ${ok}`);

    window.spikeLog = log;
    window.spikeImpl = Impl;
    window.spikeTypes = { AAC, ALAC };
  </script>
</body>
</html>
```

> **Serve the page by its explicit filename.** `/mse-spike/` alone is
> swallowed by the TanStack SPA fallback, which renders the application's
> "Link not available" screen instead. Always use `/mse-spike/index.html`.

- [ ] **Step 2: Serve it and check on desktop**

```bash
cd frontend && npm run dev
```

Open `http://localhost:3000/mse-spike/index.html`. Expected on desktop Chrome:
`MediaSource present: true` and `AAC supported: true`. `ManagedMediaSource
present` will be `false` — correct and expected off Safari.

- [ ] **Step 3: Check on the phone**

With the dev server still running, find the LAN address it prints and open
`http://<lan-ip>:3000/mse-spike/index.html` on the iPhone.

**This step answers question 1.** Record all four booleans verbatim; they go
into the results document in Task 6. If `ALAC supported` is `false` on the
phone, stop and report it before continuing — the ALAC half of the design is
dead and Tasks 4 through 6 need rescoping.

- [ ] **Step 4: Commit**

```bash
git add frontend/public/mse-spike/index.html
git commit -m "Add MSE capability probe page"
```

---

### Task 4: Trim arithmetic

The one piece worth a real test. Given the true content duration and which
append this is, compute the `timestampOffset` and append window that place each
segment exactly at the previous one's true end, with the encoder's overshoot
clipped off.

**Files:**
- Create: `frontend/public/mse-spike/trim.js`
- Create: `frontend/public/mse-spike/trim.test.js`

**Interfaces:**
- Consumes: `trueDurationPerHalf` from `fixtures.json` (Task 2).
- Produces:

```js
computeAppendPlan({ trueDuration, index }) -> {
  timestampOffset: number,   // seconds; where this append's timeline starts
  appendWindowStart: number, // seconds
  appendWindowEnd: number,   // seconds; clips the encoder overshoot
}
```

- [ ] **Step 1: Write the failing test**

Create `frontend/public/mse-spike/trim.test.js`:

```js
import { describe, expect, it } from 'vitest';
import { computeAppendPlan } from './trim.js';

describe('computeAppendPlan', () => {
  it('places the first append at the origin', () => {
    const plan = computeAppendPlan({ trueDuration: 10, index: 0 });
    expect(plan.timestampOffset).toBe(0);
    expect(plan.appendWindowStart).toBe(0);
    expect(plan.appendWindowEnd).toBe(10);
  });

  it('starts the second append exactly at the first true end', () => {
    const plan = computeAppendPlan({ trueDuration: 10, index: 1 });
    expect(plan.timestampOffset).toBe(10);
    expect(plan.appendWindowStart).toBe(10);
    expect(plan.appendWindowEnd).toBe(20);
  });

  it('clips the encoder overshoot rather than the true content', () => {
    // The AAC half's container runs 10.054240s for 10s of real audio. The
    // window must end at the true content, not at the container duration.
    const plan = computeAppendPlan({ trueDuration: 10, index: 1 });
    expect(plan.appendWindowEnd).toBe(20);
    expect(plan.appendWindowEnd).toBeLessThan(10 + 10.054240);
  });
});
```

- [ ] **Step 2: Run it and watch it fail**

```bash
cd frontend && ./node_modules/.bin/vitest run public/mse-spike/trim.test.js
```

Expected: FAIL, with a resolution error for `./trim.js`.

- [ ] **Step 3: Write the implementation**

Create `frontend/public/mse-spike/trim.js`:

```js
/**
 * Where an append lands on the shared timeline, and how much of it to keep.
 *
 * Both encoders overshoot the true content: AAC adds priming at the head and
 * padding at the tail, ALAC pads its final frame out to 4096 samples. The
 * container reports the padded duration, so appending untrimmed inserts that
 * overshoot as garbage between tracks. Append windows are expressed in
 * presentation-timeline seconds, so they carry the offset.
 */
export function computeAppendPlan({ trueDuration, index }) {
  const start = trueDuration * index;
  return {
    timestampOffset: start,
    appendWindowStart: start,
    appendWindowEnd: start + trueDuration,
  };
}
```

- [ ] **Step 4: Run it and watch it pass**

```bash
cd frontend && ./node_modules/.bin/vitest run public/mse-spike/trim.test.js
```

Expected: PASS, 3 tests.

- [ ] **Step 5: Confirm the rest of the suite still passes**

```bash
cd frontend && ./node_modules/.bin/vitest run
```

Expected: all files pass. The spike test file joins the existing suite; nothing
else changes.

- [ ] **Step 6: Commit**

```bash
git add frontend/public/mse-spike/trim.js frontend/public/mse-spike/trim.test.js
git commit -m "Add append-window arithmetic for the MSE spike"
```

---

### Task 5: Join harness

Adds the join actions to the probe page. Answers questions 2, 3, and 4.

**Files:**
- Modify: `frontend/public/mse-spike/index.html`

**Interfaces:**
- Consumes: `computeAppendPlan` from Task 4; `fixtures.json` and the fixture
  files from Tasks 1 and 2; `window.spikeLog`, `window.spikeImpl`, and
  `window.spikeTypes` from Task 3.
- Produces: nothing a later task imports. Task 6 records what it prints.

- [ ] **Step 1: Add the audio element and buttons**

In `index.html`, insert immediately after `<table id="support"></table>`:

```html
  <audio id="player" controls playsinline></audio>
  <p>
    <button data-mode="aac-untrimmed">AAC, untrimmed</button>
    <button data-mode="aac-trimmed">AAC, trimmed</button>
    <button data-mode="alac-trimmed">ALAC, trimmed</button>
    <button data-mode="alac-big">ALAC, 4min (memory)</button>
  </p>
```

- [ ] **Step 2: Import the trim helper**

In `index.html`, add this as the very first line inside the existing
`<script type="module">`, above the `const AAC = ...` line:

```js
    import { computeAppendPlan } from './trim.js';
```

- [ ] **Step 3: Add the join logic**

Append this to the end of the same `<script type="module">`, after the
`window.spikeTypes` assignment:

```js
    const audio = document.getElementById('player');
    const fixtures = await fetch('./fixtures/fixtures.json').then(r => r.json());

    const MODES = {
      'aac-untrimmed': { type: AAC, files: ['h1-aac.mp4', 'h2-aac.mp4'], trim: false },
      'aac-trimmed':   { type: AAC, files: ['h1-aac.mp4', 'h2-aac.mp4'], trim: true },
      'alac-trimmed':  { type: ALAC, files: ['h1-alac.mp4', 'h2-alac.mp4'], trim: true },
      'alac-big':      { type: ALAC, files: ['big-alac.mp4'], trim: false },
    };

    function ranges(buffered) {
      const out = [];
      for (let i = 0; i < buffered.length; i++) {
        out.push(`[${buffered.start(i).toFixed(4)}, ${buffered.end(i).toFixed(4)}]`);
      }
      return out.join(' ') || '(none)';
    }

    function once(target, event) {
      return new Promise(resolve =>
        target.addEventListener(event, resolve, { once: true }));
    }

    async function run(mode) {
      const { type, files, trim } = MODES[mode];
      log(`\n--- ${mode} ---`);

      if (!Impl || !Impl.isTypeSupported(type)) {
        log(`unsupported: ${type}`);
        return;
      }

      const ms = new Impl();
      // ManagedMediaSource attaches via srcObject and refuses to attach while
      // remote playback is enabled. Plain MediaSource needs an object URL.
      if (window.ManagedMediaSource && ms instanceof window.ManagedMediaSource) {
        audio.disableRemotePlayback = true;
        audio.srcObject = ms;
      } else {
        audio.src = URL.createObjectURL(ms);
      }

      await once(ms, 'sourceopen');
      const sb = ms.addSourceBuffer(type);
      sb.mode = trim ? 'segments' : 'sequence';

      for (let i = 0; i < files.length; i++) {
        const buf = await fetch(`./fixtures/${files[i]}`).then(r => r.arrayBuffer());
        if (trim) {
          const plan = computeAppendPlan({
            trueDuration: fixtures.trueDurationPerHalf,
            index: i,
          });
          sb.timestampOffset = plan.timestampOffset;
          sb.appendWindowStart = plan.appendWindowStart;
          sb.appendWindowEnd = plan.appendWindowEnd;
          log(`append ${files[i]} offset=${plan.timestampOffset} ` +
              `window=[${plan.appendWindowStart}, ${plan.appendWindowEnd}]`);
        } else {
          log(`append ${files[i]} (mode=sequence, no trim)`);
        }
        sb.appendBuffer(buf);
        await once(sb, 'updateend');
        log(`  buffered: ${ranges(sb.buffered)}`);
      }

      ms.endOfStream();
      log(`final buffered: ${ranges(sb.buffered)}`);
      log(`range count: ${sb.buffered.length} (1 = joined, 2 = seam)`);
      await audio.play();
    }

    for (const button of document.querySelectorAll('button[data-mode]')) {
      button.addEventListener('click', () => {
        run(button.dataset.mode).catch(err => log('ERROR: ' + err.message));
      });
    }
```

- [ ] **Step 4: Smoke test the trimmed join on desktop**

Reload `http://localhost:3000/mse-spike/index.html` and press **AAC, trimmed**.

Expected: the log prints two appends, then `range count: 1`, and a continuous
440Hz tone plays for 20 seconds. A `range count` of 2 means the appends did not
join.

- [ ] **Step 5: Compare against the untrimmed join**

Reload the page, then press **AAC, untrimmed**.

Expected: audibly worse than the trimmed run — a click or stutter at the
10-second mark. If it sounds identical to the trimmed run, that is a real
finding rather than a mistake, and it goes into the results document.

- [ ] **Step 6: Commit**

```bash
git add frontend/public/mse-spike/index.html
git commit -m "Add two-append join harness to the MSE spike"
```

---

### Task 6: Run on device and record results

The spike's actual deliverable. Nothing here is code.

**Files:**
- Create: `docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md`

**Interfaces:**
- Consumes: the harness from Task 5.
- Produces: the results document the implementation spec will be written
  against.

- [ ] **Step 1: Serve to the phone**

```bash
cd frontend && npm run dev
```

Open `http://<lan-ip>:3000/mse-spike/index.html` on the iPhone (Safari 26+). If the page
fails to attach the MediaSource over plain HTTP, note it and retry over a
tunnel — that is itself a finding worth recording.

- [ ] **Step 2: Run all four modes and capture the log**

Press each button in turn, letting each play past the 10-second mark. For each
mode, record the final `range count` and whether the join was audible. For
**ALAC, 4min**, also note whether playback survives to the end or the buffer is
evicted mid-playback.

- [ ] **Step 3: Write the results document**

Create `docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md` with the
observed values filled in — recorded, not predicted:

```markdown
# ALAC / AAC in MSE — Spike Results

Date: <date run>
Device: <model>, iOS <version>, Safari <version>
Served over: <http on LAN | tunnel>

## Capability probe

| Check | Result |
|---|---|
| ManagedMediaSource present | |
| MediaSource present | |
| AAC supported | |
| ALAC supported | |

## Joins

| Mode | Range count | Join audible? | Notes |
|---|---|---|---|
| AAC, untrimmed | | | |
| AAC, trimmed | | | |
| ALAC, trimmed | | | |
| ALAC, 4min | | | |

## Answers

1. Does ManagedMediaSource accept ALAC?
2. Do two appends join into one continuous buffered range?
3. Does the untrimmed join click?
4. Does a full-length ALAC append survive on device?

## Consequences for the implementation spec

<Which exit branch from the spike design applies, and what changes.>
```

- [ ] **Step 4: Commit the results**

```bash
git add docs/superpowers/specs/2026-08-25-alac-mse-spike-results.md
git commit -m "Record ALAC/AAC MSE spike results"
```

- [ ] **Step 5: Delete the harness**

The spike is throwaway, and the results document now carries everything learned.

```bash
git rm -r frontend/public/mse-spike
git commit -m "Remove MSE spike harness"
```

Leave the `.gitignore` entry in place: it costs nothing, and the generator is
recoverable from history if the spike ever needs rerunning.
