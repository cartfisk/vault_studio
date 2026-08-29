# Gapless Lossless Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Play consecutive lossless tracks with no audible seam, by appending them into one Media Source Extensions timeline, while every UI surface keeps showing time relative to the current track.

**Architecture:** One `PlaybackEngine` interface with two implementations. `ElementPairEngine` is today's A/B `<audio>` ping-pong moved behind the interface unchanged. `MseEngine` owns a single element with a `ManagedMediaSource` and appends consecutive tracks at running offsets. The queue-relative offset never escapes an engine — `getTrackTime()` and `seekToTrackTime()` are always track-relative.

**Tech Stack:** TypeScript, React, Vitest (node by default, jsdom via a `// @vitest-environment jsdom` pragma per file), TanStack Router, Capacitor for the Android build.

**Spec:** `docs/superpowers/specs/2026-08-26-gapless-lossless-client-design.md`. Read it first.

## Global Constraints

- **The queue-relative offset must never escape an engine.** Everything outside reads track-relative time. A leaked offset anchors waveform comments at the wrong moment — a data-correctness bug that looks like a UI glitch.
- **Codec selection is `isTypeSupported` only, never user agent.**
- **No client-side trimming, ever.** ALAC and FLAC carry no encoder priming.
- **Timeline offsets are a running sum of true durations**, computed from `sampleCount / sampleRate` as exact integer arithmetic. Never `duration * index`. Never a float read off the element.
- **`LEAD_SECONDS` is 30.** Append only when the buffer is within that of the playhead; evict beyond it behind. Appending faster than playback is what exhausted Safari's ~15MB audio quota during the spike.
- **`loop` must never be set on the MSE element** — it would loop the whole timeline.
- **`MusicPlayer.swapOrder.test.tsx` must keep passing unmodified.** It is the guard proving `ElementPairEngine` was moved, not rewritten.
- **Path alias is `@/`** mapping to `frontend/src/`. Tests import via `@/lib/...`.
- **Tests run from `frontend/` with `npm test` (`vitest run`).** Default environment is node; a file needing a DOM declares `// @vitest-environment jsdom` on its first line.
- **Indentation is tabs**, matching the existing `src/lib` files.

## Execution order

Tasks 1-5 are purely additive: new files only, no existing file modified. Tasks 6-7 refactor working playback code and are deliberately last. A partial run that stops after Task 5 leaves the app exactly as it is today with unused, tested modules on disk — safe. Do not start Task 6 unless Tasks 1-5 are committed and green.

---

## File Structure

**Create (Tasks 1-5, additive):**

| Path | Responsibility |
|---|---|
| `frontend/src/lib/playback/types.ts` | `PlaybackEngine` interface, `GaplessManifest`, `PlacedTrack` |
| `frontend/src/lib/playback/timeline.ts` | Offset arithmetic. Pure, no DOM. |
| `frontend/src/lib/playback/timeline.test.ts` | node |
| `frontend/src/lib/playback/codecSupport.ts` | `isTypeSupported` probe |
| `frontend/src/lib/playback/codecSupport.test.ts` | node |
| `frontend/src/lib/playback/selectEngine.ts` | Which engine for which track. Pure. |
| `frontend/src/lib/playback/selectEngine.test.ts` | node |
| `frontend/src/lib/playback/mseEngine.ts` | MediaSource lifecycle, append, evict |
| `frontend/src/lib/playback/mseEngine.test.ts` | jsdom, with fakes |

**Modify (Tasks 4, 6-7):**

| Path | Change |
|---|---|
| `frontend/src/api/media.ts` | `getStreamUrl` accepts `codecs`, returns optional `gapless` |
| `frontend/src/lib/playback/elementPairEngine.ts` | New file, but its content is moved from `MusicPlayer.tsx` |
| `frontend/src/components/MusicPlayer.tsx` | Transport and swap move out; renders a third `<audio>` |
| `frontend/src/contexts/AudioPlayerContext.tsx` | Fifteen reaches through `audioPlayerRef.current.audio.current` become engine calls |

---

## Task 1: Timeline arithmetic

**Files:**
- Create: `frontend/src/lib/playback/types.ts`
- Create: `frontend/src/lib/playback/timeline.ts`
- Create: `frontend/src/lib/playback/timeline.test.ts`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `interface GaplessManifest { codec: "alac" | "flac"; url: string; sampleRate: number; sampleCount: number; channels: number; initByteEnd: number; fragments: Array<{ start: number; end: number }> }`
  - `interface PlacedTrack { trackId: string; versionId: number | null; offsetSamples: number; durationSamples: number; sampleRate: number }`
  - `function placeTrack(placed: PlacedTrack[], trackId: string, versionId: number | null, manifest: GaplessManifest): PlacedTrack`
  - `function trackTimeFor(placed: PlacedTrack[], elementTime: number): { track: PlacedTrack; trackTime: number } | null`
  - `function elementTimeFor(track: PlacedTrack, trackTime: number): number`
  - `function offsetSeconds(track: PlacedTrack): number`
  - `function durationSeconds(track: PlacedTrack): number`

- [ ] **Step 1: Write the failing tests**

Create `frontend/src/lib/playback/timeline.test.ts`:

```ts
import { describe, expect, it } from "vitest";

import {
	durationSeconds,
	elementTimeFor,
	offsetSeconds,
	placeTrack,
	trackTimeFor,
} from "@/lib/playback/timeline";
import type { GaplessManifest, PlacedTrack } from "@/lib/playback/types";

function manifest(sampleCount: number, sampleRate = 44100): GaplessManifest {
	return {
		codec: "alac",
		url: "/api/stream/x/gapless/alac?version_id=1",
		sampleRate,
		sampleCount,
		channels: 2,
		initByteEnd: 710,
		fragments: [{ start: 711, end: 1000 }],
	};
}

describe("placeTrack", () => {
	it("places the first track at zero", () => {
		const t = placeTrack([], "a", 1, manifest(44100));
		expect(t.offsetSamples).toBe(0);
		expect(t.durationSamples).toBe(44100);
	});

	it("places each track at the running sum, not duration * index", () => {
		const placed: PlacedTrack[] = [];
		placed.push(placeTrack(placed, "a", 1, manifest(44100)));       // 1.0s
		placed.push(placeTrack(placed, "b", 2, manifest(66150)));       // 1.5s
		placed.push(placeTrack(placed, "c", 3, manifest(22050)));       // 0.5s

		expect(placed[0].offsetSamples).toBe(0);
		expect(placed[1].offsetSamples).toBe(44100);
		expect(placed[2].offsetSamples).toBe(110250);
	});

	it("does not accumulate float error across a long queue", () => {
		// A duration that is not exactly representable as a float fraction.
		const placed: PlacedTrack[] = [];
		for (let i = 0; i < 500; i++) {
			placed.push(placeTrack(placed, `t${i}`, i, manifest(44099)));
		}
		// Integer sample arithmetic must be exact, however long the queue.
		expect(placed[499].offsetSamples).toBe(44099 * 499);
	});
});

describe("trackTimeFor", () => {
	const placed: PlacedTrack[] = [];
	placed.push(placeTrack(placed, "a", 1, manifest(44100)));
	placed.push(placeTrack(placed, "b", 2, manifest(88200)));

	it("returns track-relative time inside the first track", () => {
		const got = trackTimeFor(placed, 0.25);
		expect(got?.track.trackId).toBe("a");
		expect(got?.trackTime).toBeCloseTo(0.25, 9);
	});

	it("attributes the exact boundary to the SECOND track", () => {
		const got = trackTimeFor(placed, 1.0);
		expect(got?.track.trackId).toBe("b");
		expect(got?.trackTime).toBeCloseTo(0, 9);
	});

	it("returns track-relative time inside a later track", () => {
		const got = trackTimeFor(placed, 2.5);
		expect(got?.track.trackId).toBe("b");
		expect(got?.trackTime).toBeCloseTo(1.5, 9);
	});

	it("returns null past the end of everything placed", () => {
		expect(trackTimeFor(placed, 99)).toBeNull();
	});

	it("returns null for an empty timeline", () => {
		expect(trackTimeFor([], 0)).toBeNull();
	});
});

describe("elementTimeFor", () => {
	it("is the inverse of trackTimeFor", () => {
		const placed: PlacedTrack[] = [];
		placed.push(placeTrack(placed, "a", 1, manifest(44100)));
		placed.push(placeTrack(placed, "b", 2, manifest(88200)));

		const elementTime = elementTimeFor(placed[1], 1.5);
		expect(elementTime).toBeCloseTo(2.5, 9);

		const back = trackTimeFor(placed, elementTime);
		expect(back?.track.trackId).toBe("b");
		expect(back?.trackTime).toBeCloseTo(1.5, 9);
	});
});

describe("offsetSeconds and durationSeconds", () => {
	it("convert samples to seconds at the track's own rate", () => {
		const placed: PlacedTrack[] = [];
		placed.push(placeTrack(placed, "a", 1, manifest(48000, 48000)));
		placed.push(placeTrack(placed, "b", 2, manifest(96000, 48000)));

		expect(offsetSeconds(placed[1])).toBeCloseTo(1.0, 9);
		expect(durationSeconds(placed[1])).toBeCloseTo(2.0, 9);
	});
});
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd frontend && npm test -- timeline
```

Expected: FAIL — the modules do not exist.

- [ ] **Step 3: Write the types**

Create `frontend/src/lib/playback/types.ts`:

```ts
/** A byte range from the backend manifest. Both ends are INCLUSIVE, matching
 *  HTTP `Range: bytes=start-end`. The client never does byte arithmetic on
 *  these — they are used verbatim. */
export interface FragmentRange {
	start: number;
	end: number;
}

/** The `gapless` object returned by GET /api/media/stream/{id}?codecs=... */
export interface GaplessManifest {
	codec: "alac" | "flac";
	/** Already carries its own version_id. Use verbatim; do not rebuild it. */
	url: string;
	sampleRate: number;
	sampleCount: number;
	channels: number;
	/** Inclusive last byte of the ftyp+moov prelude. */
	initByteEnd: number;
	fragments: FragmentRange[];
}

/** One track's position on the shared MSE timeline.
 *
 *  Offsets and durations are in SAMPLES, not seconds, so a long queue cannot
 *  accumulate float error. Seconds are derived only at the boundary, by
 *  offsetSeconds/durationSeconds. */
export interface PlacedTrack {
	trackId: string;
	versionId: number | null;
	offsetSamples: number;
	durationSamples: number;
	sampleRate: number;
}
```

- [ ] **Step 4: Write the timeline arithmetic**

Create `frontend/src/lib/playback/timeline.ts`:

```ts
import type { GaplessManifest, PlacedTrack } from "@/lib/playback/types";

/**
 * Position a track after everything already placed.
 *
 * The offset is a running sum of true sample counts. It is NOT
 * `duration * index` — track durations vary, and that mistake puts every
 * later track in the queue at the wrong place.
 */
export function placeTrack(
	placed: PlacedTrack[],
	trackId: string,
	versionId: number | null,
	manifest: GaplessManifest,
): PlacedTrack {
	const last = placed[placed.length - 1];
	const offsetSamples = last ? last.offsetSamples + last.durationSamples : 0;

	return {
		trackId,
		versionId,
		offsetSamples,
		durationSamples: manifest.sampleCount,
		sampleRate: manifest.sampleRate,
	};
}

export function offsetSeconds(track: PlacedTrack): number {
	return track.offsetSamples / track.sampleRate;
}

export function durationSeconds(track: PlacedTrack): number {
	return track.durationSamples / track.sampleRate;
}

/**
 * Map a position on the shared element timeline back to a track and a
 * track-relative time.
 *
 * This is the function that keeps the offset from escaping. Everything the UI
 * displays comes through here.
 *
 * A position exactly on a boundary belongs to the LATER track: at the instant
 * track A's last sample has played, the playhead is at track B's first.
 */
export function trackTimeFor(
	placed: PlacedTrack[],
	elementTime: number,
): { track: PlacedTrack; trackTime: number } | null {
	for (let i = placed.length - 1; i >= 0; i--) {
		const track = placed[i];
		const start = offsetSeconds(track);
		if (elementTime >= start) {
			const trackTime = elementTime - start;
			if (trackTime > durationSeconds(track)) return null;
			return { track, trackTime };
		}
	}
	return null;
}

/** Inverse of trackTimeFor: where on the element timeline a track-relative
 *  position lives. Used for seeking. */
export function elementTimeFor(track: PlacedTrack, trackTime: number): number {
	return offsetSeconds(track) + trackTime;
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
cd frontend && npm test -- timeline
```

Expected: PASS, all cases.

- [ ] **Step 6: Prove the running-sum test discriminates**

Temporarily change `placeTrack` to use `placed.length * manifest.sampleCount` as the offset. Run the tests again — "places each track at the running sum" must FAIL. Restore, confirm PASS. Quote both outputs in the report.

This matters because `duration * index` is the specific mistake the spike results call out, and a test that passes either way would not catch it.

- [ ] **Step 7: Commit**

```bash
git add frontend/src/lib/playback/types.ts frontend/src/lib/playback/timeline.ts frontend/src/lib/playback/timeline.test.ts
git commit -m "Add gapless timeline arithmetic

- offsets are a running sum of true sample counts, never duration * index
- samples not seconds, so a long queue cannot accumulate float error
- trackTimeFor is what keeps queue-relative time from reaching the UI"
```

---

## Task 2: Codec support probe

**Files:**
- Create: `frontend/src/lib/playback/codecSupport.ts`
- Create: `frontend/src/lib/playback/codecSupport.test.ts`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `const LOSSLESS_MIME: Record<"alac" | "flac", string>`
  - `function supportedLosslessCodecs(impl?: typeof MediaSource): Array<"alac" | "flac">`
  - `function codecsParam(impl?: typeof MediaSource): string | null`

- [ ] **Step 1: Write the failing tests**

Create `frontend/src/lib/playback/codecSupport.test.ts`:

```ts
import { describe, expect, it } from "vitest";

import { codecsParam, LOSSLESS_MIME, supportedLosslessCodecs } from "@/lib/playback/codecSupport";

/** A stand-in for MediaSource that supports exactly the listed MIME types. */
function fakeImpl(supported: string[]) {
	return {
		isTypeSupported: (type: string) => supported.includes(type),
	} as unknown as typeof MediaSource;
}

describe("supportedLosslessCodecs", () => {
	it("reports alac when only alac is supported", () => {
		expect(supportedLosslessCodecs(fakeImpl([LOSSLESS_MIME.alac]))).toEqual(["alac"]);
	});

	it("reports flac when only flac is supported", () => {
		expect(supportedLosslessCodecs(fakeImpl([LOSSLESS_MIME.flac]))).toEqual(["flac"]);
	});

	it("reports both, alac first, when both are supported", () => {
		expect(
			supportedLosslessCodecs(fakeImpl([LOSSLESS_MIME.alac, LOSSLESS_MIME.flac])),
		).toEqual(["alac", "flac"]);
	});

	it("reports nothing when neither is supported", () => {
		expect(supportedLosslessCodecs(fakeImpl([]))).toEqual([]);
	});

	it("reports nothing when there is no MediaSource at all", () => {
		expect(supportedLosslessCodecs(undefined)).toEqual([]);
	});
});

describe("codecsParam", () => {
	it("joins supported codecs in preference order", () => {
		expect(codecsParam(fakeImpl([LOSSLESS_MIME.alac, LOSSLESS_MIME.flac]))).toBe("alac,flac");
	});

	it("returns null rather than an empty string when nothing is supported", () => {
		// An empty codecs param would still opt into the gapless branch on the
		// server. Absent means absent.
		expect(codecsParam(fakeImpl([]))).toBeNull();
	});
});
```

- [ ] **Step 2: Run to verify failure**

```bash
cd frontend && npm test -- codecSupport
```

Expected: FAIL — module does not exist.

- [ ] **Step 3: Implement**

Create `frontend/src/lib/playback/codecSupport.ts`:

```ts
export type LosslessCodec = "alac" | "flac";

/** ALAC plays on Safari, FLAC on Chrome and Firefox. Neither engine supports
 *  the other, which is why the backend stores both. */
export const LOSSLESS_MIME: Record<LosslessCodec, string> = {
	alac: 'audio/mp4; codecs="alac"',
	flac: 'audio/mp4; codecs="flac"',
};

/** Preference order. ALAC is first so Safari, which supports only ALAC, is
 *  never asked to consider anything else. */
const ORDER: LosslessCodec[] = ["alac", "flac"];

function defaultImpl(): typeof MediaSource | undefined {
	if (typeof window === "undefined") return undefined;
	const w = window as unknown as {
		ManagedMediaSource?: typeof MediaSource;
		MediaSource?: typeof MediaSource;
	};
	return w.ManagedMediaSource ?? w.MediaSource;
}

/**
 * Which lossless codecs this browser can actually play through MSE.
 *
 * Decided by isTypeSupported and never by user agent. User-agent sniffing is
 * how this breaks on the next browser release.
 */
export function supportedLosslessCodecs(
	impl: typeof MediaSource | undefined = defaultImpl(),
): LosslessCodec[] {
	if (!impl || typeof impl.isTypeSupported !== "function") return [];
	return ORDER.filter((codec) => impl.isTypeSupported(LOSSLESS_MIME[codec]));
}

/**
 * The value for the API's `codecs` parameter, or null when there is nothing
 * to ask for.
 *
 * Null, not an empty string: the server treats a present-but-empty `codecs`
 * as an opt-in to the gapless branch, and an unsupported browser must not
 * opt in at all.
 */
export function codecsParam(
	impl: typeof MediaSource | undefined = defaultImpl(),
): string | null {
	const codecs = supportedLosslessCodecs(impl);
	return codecs.length > 0 ? codecs.join(",") : null;
}
```

- [ ] **Step 4: Run to verify pass**

```bash
cd frontend && npm test -- codecSupport
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/playback/codecSupport.ts frontend/src/lib/playback/codecSupport.test.ts
git commit -m "Add lossless codec support probe

- isTypeSupported only, never user agent
- ManagedMediaSource preferred, plain MediaSource as fallback
- null rather than an empty codecs param when nothing is supported"
```

---

## Task 3: Engine selection

**Files:**
- Create: `frontend/src/lib/playback/selectEngine.ts`
- Create: `frontend/src/lib/playback/selectEngine.test.ts`

**Interfaces:**
- Consumes: `GaplessManifest` from Task 1, `LosslessCodec` from Task 2
- Produces:
  - `type EngineKind = "mse" | "elementPair"`
  - `function selectEngine(input: { manifest?: GaplessManifest | null; supported: LosslessCodec[] }): EngineKind`
  - `function canAppendNext(current: EngineKind, next: EngineKind): boolean`

- [ ] **Step 1: Write the failing tests**

Create `frontend/src/lib/playback/selectEngine.test.ts`:

```ts
import { describe, expect, it } from "vitest";

import { canAppendNext, selectEngine } from "@/lib/playback/selectEngine";
import type { GaplessManifest } from "@/lib/playback/types";

function manifest(codec: "alac" | "flac"): GaplessManifest {
	return {
		codec,
		url: `/api/stream/x/gapless/${codec}?version_id=1`,
		sampleRate: 44100,
		sampleCount: 44100,
		channels: 2,
		initByteEnd: 710,
		fragments: [{ start: 711, end: 1000 }],
	};
}

describe("selectEngine", () => {
	it("chooses mse when the manifest's codec is supported", () => {
		expect(selectEngine({ manifest: manifest("alac"), supported: ["alac"] })).toBe("mse");
	});

	it("falls back when there is no manifest", () => {
		expect(selectEngine({ manifest: null, supported: ["alac", "flac"] })).toBe("elementPair");
		expect(selectEngine({ supported: ["alac"] })).toBe("elementPair");
	});

	it("falls back when the browser supports nothing", () => {
		expect(selectEngine({ manifest: manifest("alac"), supported: [] })).toBe("elementPair");
	});

	it("refuses a manifest for a codec this browser cannot play", () => {
		// The server should never send this, but trusting it would produce
		// silence rather than a seam.
		expect(selectEngine({ manifest: manifest("flac"), supported: ["alac"] })).toBe("elementPair");
	});
});

describe("canAppendNext", () => {
	it("appends only when both tracks are on the mse engine", () => {
		expect(canAppendNext("mse", "mse")).toBe(true);
	});

	it("refuses to append across an engine change", () => {
		expect(canAppendNext("mse", "elementPair")).toBe(false);
		expect(canAppendNext("elementPair", "mse")).toBe(false);
		expect(canAppendNext("elementPair", "elementPair")).toBe(false);
	});
});
```

- [ ] **Step 2: Run to verify failure**

```bash
cd frontend && npm test -- selectEngine
```

Expected: FAIL — module does not exist.

- [ ] **Step 3: Implement**

Create `frontend/src/lib/playback/selectEngine.ts`:

```ts
import type { LosslessCodec } from "@/lib/playback/codecSupport";
import type { GaplessManifest } from "@/lib/playback/types";

export type EngineKind = "mse" | "elementPair";

/**
 * Which engine plays this track.
 *
 * The server decides availability — it sends a manifest only when the
 * resolved quality is lossless and a completed segment set exists. The client
 * decides capability. Both must agree.
 */
export function selectEngine(input: {
	manifest?: GaplessManifest | null;
	supported: LosslessCodec[];
}): EngineKind {
	const { manifest, supported } = input;
	if (!manifest) return "elementPair";
	if (!supported.includes(manifest.codec)) return "elementPair";
	return "mse";
}

/**
 * Whether the next track can join the current timeline without a teardown.
 *
 * Only an MSE-to-MSE transition is gapless. Everything else tears one engine
 * down and starts another, which costs the same seam the lossy tier has
 * always had. That is the accepted cost of a mixed queue.
 */
export function canAppendNext(current: EngineKind, next: EngineKind): boolean {
	return current === "mse" && next === "mse";
}
```

- [ ] **Step 4: Run to verify pass**

```bash
cd frontend && npm test -- selectEngine
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/playback/selectEngine.ts frontend/src/lib/playback/selectEngine.test.ts
git commit -m "Add per-track playback engine selection

- server decides availability, client decides capability
- a manifest for an unsupported codec falls back rather than trusting it
- only mse-to-mse can append without a seam"
```

---

## Task 4: Manifest fetching

**Files:**
- Modify: `frontend/src/api/media.ts`
- Create: `frontend/src/api/media.test.ts`

**Interfaces:**
- Consumes: `GaplessManifest` from Task 1, `codecsParam` from Task 2
- Produces: `getStreamUrl(trackId, params?: { quality?: string; versionId?: number | null; codecs?: string | null }): Promise<{ url: string; gapless?: GaplessManifest }>`

- [ ] **Step 1: Read the current implementation**

```bash
sed -n '32,46p' frontend/src/api/media.ts
```

It builds a `URLSearchParams` from `quality` and `versionId` and calls `get<{ url: string }>`. You are widening the return type and adding one optional parameter. Do not change the existing parameters' behaviour.

- [ ] **Step 2: Write the failing test**

Create `frontend/src/api/media.test.ts`:

```ts
import { describe, expect, it, vi, beforeEach } from "vitest";

const get = vi.fn();
vi.mock("@/api/client", () => ({ get: (...args: unknown[]) => get(...args) }));

const { getStreamUrl } = await import("@/api/media");

beforeEach(() => {
	get.mockReset();
	get.mockResolvedValue({ url: "/api/stream/abc?sig=x" });
});

describe("getStreamUrl", () => {
	it("omits the codecs param entirely when none is given", async () => {
		await getStreamUrl("abc");
		expect(get).toHaveBeenCalledWith("/api/media/stream/abc");
	});

	it("omits the codecs param when it is null", async () => {
		// An unsupported browser must not opt into the gapless branch at all.
		await getStreamUrl("abc", { codecs: null });
		expect(get).toHaveBeenCalledWith("/api/media/stream/abc");
	});

	it("sends codecs when supported", async () => {
		await getStreamUrl("abc", { codecs: "alac,flac" });
		expect(get).toHaveBeenCalledWith("/api/media/stream/abc?codecs=alac%2Cflac");
	});

	it("passes the gapless manifest through untouched", async () => {
		const gapless = {
			codec: "alac" as const,
			url: "/api/stream/abc/gapless/alac?version_id=7",
			sampleRate: 44100,
			sampleCount: 1102500,
			channels: 2,
			initByteEnd: 710,
			fragments: [{ start: 711, end: 5000 }],
		};
		get.mockResolvedValue({ url: "/api/stream/abc?sig=x", gapless });

		const res = await getStreamUrl("abc", { codecs: "alac" });
		expect(res.gapless).toEqual(gapless);
	});

	it("returns no manifest when the server sends none", async () => {
		const res = await getStreamUrl("abc", { codecs: "alac" });
		expect(res.gapless).toBeUndefined();
	});
});
```

- [ ] **Step 3: Run to verify failure**

```bash
cd frontend && npm test -- media
```

Expected: FAIL — `codecs` is not a parameter yet, so the query string assertions fail.

- [ ] **Step 4: Implement**

In `frontend/src/api/media.ts`, import the manifest type and widen the signature:

```ts
import type { GaplessManifest } from "@/lib/playback/types";
```

Change `getStreamUrl` to accept `codecs?: string | null` and set it on the query only when it is a non-empty string, and to return `Promise<{ url: string; gapless?: GaplessManifest }>`. Leave `quality` and `versionId` handling exactly as they are.

- [ ] **Step 5: Run to verify pass**

```bash
cd frontend && npm test -- media
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/api/media.ts frontend/src/api/media.test.ts
git commit -m "Return the gapless manifest from getStreamUrl

- codecs is optional and omitted entirely when null
- an empty codecs param would opt into the gapless branch, so never send one
- existing quality and versionId behaviour is unchanged"
```

---

## Task 5: MSE engine

**Files:**
- Create: `frontend/src/lib/playback/mseEngine.ts`
- Create: `frontend/src/lib/playback/mseEngine.test.ts`

This is the largest task. Its tests use fakes for `MediaSource`, `SourceBuffer`, and the media element — the point is control flow, not media decoding.

**Interfaces:**
- Consumes: everything from Tasks 1-4
- Produces:
  - `const LEAD_SECONDS = 30`
  - `function createMseEngine(deps: MseEngineDeps): PlaybackEngine`
  - `interface MseEngineDeps { element: HTMLAudioElement; mediaSourceImpl: typeof MediaSource; fetchRange(url: string, start: number, end: number): Promise<ArrayBuffer> }`

Dependencies are injected rather than reached for, so the tests can supply fakes without a real browser.

- [ ] **Step 1: Write the failing tests**

Create `frontend/src/lib/playback/mseEngine.test.ts` with `// @vitest-environment jsdom` on the first line.

Cover at minimum:

- **Backpressure.** With a fake element whose `currentTime` never advances, the append loop stops after the buffer is `LEAD_SECONDS` ahead rather than appending every fragment. Assert the number of `fetchRange` calls is bounded, not equal to the fragment count. **This is the regression that reintroduces the quota failure the spike measured.**
- **Eviction.** As the fake element's `currentTime` advances, `remove` is called for the range behind `currentTime - LEAD_SECONDS`, and never for a range ahead of `currentTime`.
- **Init append.** `bytes=0-initByteEnd` is appended exactly once per track, before any fragment.
- **Teardown stops a waiting loop.** Start an append loop that is waiting on `timeupdate`, call `teardown()`, and assert the loop settles rather than hanging and that no further `fetchRange` happens.
- **Track-relative time.** After placing two tracks, `getTrackTime()` returns time relative to the current track, not the timeline.
- **Fetch failure.** A rejected `fetchRange` surfaces as an `error` event to the subscriber rather than an unhandled rejection.

- [ ] **Step 2: Run to verify failure**

```bash
cd frontend && npm test -- mseEngine
```

Expected: FAIL — module does not exist.

- [ ] **Step 3: Implement `createMseEngine`**

Structure it as: attach (`disableRemotePlayback = true`, `srcObject = new impl()`, await `sourceopen`), then `load`/`prepareNext` place a track via `placeTrack`, set `sourceBuffer.timestampOffset` to `offsetSeconds(track)`, append the init range once, then run the append loop.

The append loop is the part that must be right:

```ts
// Append only when the buffer is running low. Appending faster than
// playback is what exhausted Safari's audio quota during the spike: the
// harness raced to append 37MB and died at 15MB.
while (index < fragments.length && !stopped) {
	while (bufferedAhead() > LEAD_SECONDS && !stopped) {
		await waitFor(element, "timeupdate");
	}
	if (stopped) return;
	await appendOne(fragments[index++]);
	await evictBehind();
}
```

`stopped` is set by `teardown()` and every wait must observe it, or teardown leaves a loop awaiting an event that will never fire on a paused element.

Emit `trackchange` from a `timeupdate` handler when `trackTimeFor` reports a different track than the current one — no `ended` event fires at a gapless boundary, which is the entire point.

Never set `loop` on the element. Loop mode seeks to `elementTimeFor(current, 0)`.

- [ ] **Step 4: Run to verify pass**

```bash
cd frontend && npm test -- mseEngine
```

Expected: PASS.

- [ ] **Step 5: Prove the backpressure test discriminates**

Remove the inner `while (bufferedAhead() > LEAD_SECONDS)` wait so the loop appends unconditionally. The backpressure test must FAIL. Restore, confirm PASS. Quote both outputs.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/playback/mseEngine.ts frontend/src/lib/playback/mseEngine.test.ts
git commit -m "Add the MSE gapless playback engine

- appends consecutive tracks at running offsets on one timeline
- backpressure and eviction keep the buffer inside Safari's quota
- trackchange is emitted from timeupdate; no ended fires at a gapless join
- getTrackTime is always track-relative"
```

---

## Task 6: Element-pair engine

**Files:**
- Create: `frontend/src/lib/playback/elementPairEngine.ts`
- Modify: `frontend/src/components/MusicPlayer.tsx`

**This task modifies working playback code.** Do not start it until Tasks 1-5 are committed and the suite is green.

The content is MOVED from `MusicPlayer.tsx`, not rewritten: the A/B swap, the iOS gesture unlock, and the preload handling. Both carry comments explaining non-obvious failures they were written to fix — preserve those comments with the code they explain.

`canAppend()` always returns false. Every track change is a swap.

**The guard:** `frontend/src/components/MusicPlayer.swapOrder.test.tsx` must keep passing **unmodified**. If it needs editing, the behaviour changed and this is no longer a move — stop and report rather than adjusting the test.

- [ ] **Step 1: Confirm the guard passes before you start**

```bash
cd frontend && npm test -- swapOrder
```

Expected: PASS. Record the output.

- [ ] **Step 2: Move the swap and unlock logic behind the interface**

Extract into `elementPairEngine.ts`, keeping the existing comments. `MusicPlayer.tsx` keeps rendering the elements and passes their refs to the engine.

- [ ] **Step 3: Verify the guard still passes, unmodified**

```bash
cd frontend && git diff --stat src/components/MusicPlayer.swapOrder.test.tsx && npm test -- swapOrder
```

Expected: no diff on the test file, and PASS. A diff means stop and report.

- [ ] **Step 4: Run the whole suite**

```bash
cd frontend && npm test
```

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/playback/elementPairEngine.ts frontend/src/components/MusicPlayer.tsx
git commit -m "Move A/B playback behind the PlaybackEngine interface

- swap ordering and iOS gesture unlock moved verbatim, comments intact
- canAppend is always false: every track change stays a swap
- swapOrder test passes unmodified, which is the guard on 'moved'"
```

---

## Task 7: Integration

**Files:**
- Modify: `frontend/src/components/MusicPlayer.tsx`
- Modify: `frontend/src/contexts/AudioPlayerContext.tsx`

**This is the riskiest task.** `audioPlayerRef` is typed `any`, so the compiler will not find every call site. Enumerate them by grep before editing:

```bash
cd frontend && grep -n "audioPlayerRef.current" src/contexts/AudioPlayerContext.tsx
```

Every one becomes an engine call:

| Today | Becomes |
|---|---|
| `.audio.current.pause()` | `engine.pause()` |
| `.audio.current.play()` | `engine.play()` |
| `.audio.current.currentTime` (read) | `engine.getTrackTime()` |
| `.audio.current.currentTime = t` | `engine.seekToTrackTime(t)` |
| `.audio.current.src = ""` | `engine.teardown()` |

- [ ] **Step 1: Render the third element**

`MusicPlayer.tsx` renders a third `<audio>` for MSE alongside the A/B pair. Only this one sets `disableRemotePlayback`. It must never have `loop` set.

- [ ] **Step 2: Extend the iOS unlock to the MSE element**

On the same user gesture that unlocks the standby, attach the MediaSource to the MSE element and play it muted briefly.

Attaching `srcObject` is what makes `play()` resolvable, so no silent data URI is needed — but the gesture is still required. Unlocking lazily at the first lossless track will fail: iOS refuses to buffer an element that has never played inside a gesture, and the track silently will not play.

- [ ] **Step 3: Route engine selection through the preload trigger**

`shouldStartPreload` keeps deciding when. On fire, fetch the next track's manifest, `selectEngine`, and either `prepareNext` on the current engine or prepare a handoff.

- [ ] **Step 4: Replace the context's direct reaches**

Change all fifteen call sites per the table above. `audioPlayerRef` exposes the engine, not a raw element.

- [ ] **Step 5: Wire MediaSession to engine events**

`setPositionState` takes `engine.getTrackTime()` and `engine.getTrackDuration()`. Lock-screen metadata updates on the engine's `trackchange`.

- [ ] **Step 6: Run the whole suite**

```bash
cd frontend && npm test && npx tsc --noEmit
```

- [ ] **Step 7: Commit**

```bash
git add frontend/src/components/MusicPlayer.tsx frontend/src/contexts/AudioPlayerContext.tsx
git commit -m "Drive playback through the engine interface

- the context talks to an engine, never to a raw media element
- track-relative time is the only time any UI surface sees
- MSE element unlocked on the same gesture as the standby"
```

---

## Device verification — NOT automatable

The plan is not done when the suite is green. These require a physical iPhone and a human listener, and nothing above substitutes for them:

1. An all-lossless album plays with no audible seam.
2. A lossless → MP3 boundary degrades to today's seam, not silence or a stall.
3. The first lossless track after a cold start plays. **Most likely failure — the iOS unlock.**
4. Lock screen shows the right track at a gapless boundary.
5. AirPlay via Control Center keeps working across a track transition.
6. Scrubbing, and waveform comments landing at the right moment.

Item 6 is the one no test here covers, and a leaked offset there is a data-correctness bug that looks like a UI glitch.

## Done when

- `npm test` green, `npx tsc --noEmit` clean.
- `MusicPlayer.swapOrder.test.tsx` passes unmodified.
- Every new test proven to fail when the behaviour it covers is broken.
- The six device checks above have been run by a human.
