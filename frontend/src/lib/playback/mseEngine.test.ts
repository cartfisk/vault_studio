// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";

import { LEAD_SECONDS, createMseEngine } from "@/lib/playback/mseEngine";
import type { FragmentRange, GaplessManifest, PlayableTrack } from "@/lib/playback/types";

/**
 * jsdom's <audio> element does not implement `srcObject`, so the fake
 * element below is a plain `EventTarget` subclass rather than a real
 * `HTMLAudioElement`. It's cast to `HTMLAudioElement` at the call site.
 */
class FakeElement extends EventTarget {
	currentTime = 0;
	volume = 1;
	disableRemotePlayback = false;
	srcObject: unknown = null;
	played = false;

	private listenerCounts = new Map<string, number>();

	addEventListener(type: string, listener: EventListenerOrEventListenerObject | null): void {
		super.addEventListener(type, listener);
		this.listenerCounts.set(type, (this.listenerCounts.get(type) ?? 0) + 1);
	}

	removeEventListener(type: string, listener: EventListenerOrEventListenerObject | null): void {
		super.removeEventListener(type, listener);
		this.listenerCounts.set(type, Math.max(0, (this.listenerCounts.get(type) ?? 0) - 1));
	}

	/** How many listeners are currently registered for `type`. Used to prove
	 *  teardown() actually cleaned up a pending wait, not just that it
	 *  stopped generating fetches. */
	listenerCount(type: string): number {
		return this.listenerCounts.get(type) ?? 0;
	}

	async play(): Promise<void> {
		this.played = true;
	}

	pause(): void {
		this.played = false;
	}

	/** Test helper: move the playhead and fire the event the append loop
	 *  waits on, exactly like a real element does during playback. */
	advanceTime(t: number): void {
		this.currentTime = t;
		this.dispatchEvent(new Event("timeupdate"));
	}

	/** Test helper: a user seek. Real elements fire `seeking` synchronously
	 *  when currentTime is assigned. */
	seek(t: number): void {
		this.currentTime = t;
		this.dispatchEvent(new Event("seeking"));
	}
}

/** Records appendBuffer/remove calls and fires `updateend` asynchronously,
 *  like a real SourceBuffer. Each appended chunk grows the buffered range by
 *  a fixed, test-controlled number of seconds — the fakes exist to exercise
 *  control flow, not to decode media. */
class FakeSourceBuffer extends EventTarget {
	mode = "";
	appendCalls: ArrayBuffer[] = [];
	removeCalls: Array<{ start: number; end: number }> = [];
	ranges: Array<{ start: number; end: number }> = [];
	updating = false;
	/** When true, `updateend` is held until `finishPending()`, so a test can
	 *  observe what the engine does while an append/remove is in flight. */
	holdUpdates = false;

	private pending: Array<() => void> = [];
	private offset = 0;

	get timestampOffset(): number {
		return this.offset;
	}

	/** Like a real SourceBuffer, refuses a new offset mid-append/remove. */
	set timestampOffset(v: number) {
		if (this.updating) throw new DOMException("timestampOffset set while updating", "InvalidStateError");
		this.offset = v;
	}

	constructor(private secondsPerAppend: number) {
		super();
	}

	private complete(): void {
		this.updating = true;
		const finish = () => {
			this.updating = false;
			this.dispatchEvent(new Event("updateend"));
		};
		if (this.holdUpdates) this.pending.push(finish);
		else queueMicrotask(finish);
	}

	/** Stops holding and completes every held append/remove. */
	finishPending(): void {
		this.holdUpdates = false;
		const pending = this.pending;
		this.pending = [];
		for (const finish of pending) finish();
	}

	get buffered() {
		const ranges = this.ranges;
		return {
			length: ranges.length,
			start: (i: number) => ranges[i].start,
			end: (i: number) => ranges[i].end,
		};
	}

	/** Appends grow the range that starts at `timestampOffset`, or open a
	 *  new one, like a real buffer after a seek. */
	appendBuffer(data: ArrayBuffer): void {
		this.appendCalls.push(data);
		const at = this.timestampOffset;
		const range = this.ranges.find((r) => r.start <= at && at <= r.end);
		if (range) range.end += this.secondsPerAppend;
		else this.ranges.push({ start: at, end: at + this.secondsPerAppend });
		this.ranges.sort((a, b) => a.start - b.start);
		this.complete();
	}

	remove(start: number, end: number): void {
		this.removeCalls.push({ start, end });
		this.ranges = this.ranges
			.map((r) => (r.start >= start && r.end <= end ? null : r.start < start && r.end > start ? { ...r, end: Math.min(r.end, start) } : r.start < end && r.end > end ? { ...r, start: Math.max(r.start, end) } : r))
			.filter((r): r is { start: number; end: number } => r !== null);
		this.complete();
	}
}

class FakeMediaSource extends EventTarget {
	sourceBuffers: FakeSourceBuffer[] = [];

	constructor(private secondsPerAppend = 10) {
		super();
		// A real MediaSource fires sourceopen asynchronously once attached.
		queueMicrotask(() => this.dispatchEvent(new Event("sourceopen")));
	}

	addSourceBuffer(_mime: string): FakeSourceBuffer {
		const sb = new FakeSourceBuffer(this.secondsPerAppend);
		this.sourceBuffers.push(sb);
		return sb;
	}
}

function manifest(overrides: Partial<GaplessManifest> = {}): GaplessManifest {
	const fragmentCount = overrides.fragments ? overrides.fragments.length : 10;
	const fragments: FragmentRange[] =
		overrides.fragments ??
		Array.from({ length: fragmentCount }, (_, i) => ({
			start: 1000 + i * 100,
			end: 1000 + i * 100 + 99,
		}));

	return {
		codec: "alac",
		url: "/api/stream/track/gapless/alac?version_id=1",
		sampleRate: 44100,
		sampleCount: 44100,
		channels: 2,
		initByteEnd: 710,
		...overrides,
		fragments,
	};
}

function playableTrack(
	trackId: string,
	versionId: number | null,
	overrides: Partial<GaplessManifest> = {},
): PlayableTrack {
	const m = manifest(overrides);
	return { trackId, versionId, url: m.url, manifest: m };
}

/**
 * Wait until `observe()` stops changing between ticks, instead of assuming
 * a fixed number of ticks is enough. A fixed-tick flush can silently freeze
 * the loop mid-run and report whatever count happened to exist at that
 * point — which looks identical to a working backpressure guard even when
 * the guard is missing entirely, if the two states are indistinguishable at
 * that particular tick. Polling to quiescence (with a hard cap so a real
 * hang fails loudly instead of hanging the suite) reports the loop's actual
 * settled state: either genuinely blocked waiting on `timeupdate`, or
 * genuinely finished.
 */
async function flushUntilQuiescent(observe: () => number, maxTicks = 50): Promise<void> {
	let last = observe();
	for (let i = 0; i < maxTicks; i++) {
		await new Promise<void>((resolve) => setTimeout(resolve, 0));
		const current = observe();
		if (current === last) return;
		last = current;
	}
	throw new Error(`flushUntilQuiescent: still changing after ${maxTicks} ticks (last=${last})`);
}

function makeEngine(opts: { secondsPerAppend?: number; fetchRange?: ReturnType<typeof vi.fn> } = {}) {
	const element = new FakeElement();
	const fetchRange =
		opts.fetchRange ?? vi.fn(async (_url: string, _start: number, _end: number) => new ArrayBuffer(8));
	const mediaSourceImpl = class extends FakeMediaSource {
		constructor() {
			super(opts.secondsPerAppend ?? 10);
		}
	};

	const engine = createMseEngine({
		element: element as unknown as HTMLAudioElement,
		mediaSourceImpl: mediaSourceImpl as unknown as typeof MediaSource,
		fetchRange,
	});

	return { element, fetchRange, engine };
}

describe("createMseEngine", () => {
	// LEAD_SECONDS is a design constant the engine must actually use; assert
	// it rather than hardcoding 30 everywhere below.
	it("uses a 30 second lead", () => {
		expect(LEAD_SECONDS).toBe(30);
	});

	it("stops appending once the buffer is LEAD_SECONDS ahead, instead of appending every fragment", async () => {
		// 10 fragments, 10s per appended chunk, currentTime pinned at 0.
		// Unbounded appending would call fetchRange 11 times (init + 10
		// fragments). With backpressure it must stop well short of that.
		const { fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });

		await engine.load(playableTrack("a", 1, { fragments: Array.from({ length: 10 }, (_, i) => ({ start: i, end: i })) }));
		await flushUntilQuiescent(() => fetchRange.mock.calls.length);

		expect(fetchRange.mock.calls.length).toBeGreaterThan(0);
		expect(fetchRange.mock.calls.length).toBeLessThan(11);
	});

	it("also applies backpressure before appending a queued next track's init bytes, not only its fragments", async () => {
		// Track "a" has exactly one fragment, sized (with secondsPerAppend)
		// so that by the time its init + one fragment have landed, the
		// buffer is already past LEAD_SECONDS. The very next decision the
		// loop makes is whether to append track "b"'s init bytes — this is
		// the ONLY place that decision is observable in isolation, because
		// in a single-track queue the init append always happens at
		// bufferedAhead() === 0 and can never be blocked, no matter whether
		// its own guard is present. A test that only ever loads one track
		// cannot tell a working init-append guard from a missing one.
		const { fetchRange, engine } = makeEngine({ secondsPerAppend: 20 });

		await engine.load(playableTrack("a", 1, { fragments: [{ start: 0, end: 0 }] }));
		await engine.prepareNext(playableTrack("b", 2, { fragments: [{ start: 0, end: 0 }] }));
		await flushUntilQuiescent(() => fetchRange.mock.calls.length);

		// init(a)=20s, frag0(a)=40s -> bufferedAhead is 40, over the 30s
		// lead, so track b's init must not be fetched yet.
		expect(fetchRange.mock.calls.length).toBe(2);
	});

	it("evicts buffered data behind currentTime - LEAD_SECONDS, and never ahead of currentTime", async () => {
		const { element, engine, fetchRange } = makeEngine({ secondsPerAppend: 10 });

		await engine.load(playableTrack("a", 1));
		await flushUntilQuiescent(() => fetchRange.mock.calls.length);

		// currentTime pinned at 0 so far: nothing should have been evicted
		// yet (cutoff would be negative).
		expect(element.listenerCount("timeupdate")).toBeGreaterThan(0);

		element.advanceTime(40);
		await flushUntilQuiescent(() => fetchRange.mock.calls.length);

		const removeCalls = (element.srcObject as FakeMediaSource).sourceBuffers[0].removeCalls;
		expect(removeCalls.length).toBeGreaterThan(0);
		for (const call of removeCalls) {
			// cutoff = currentTime - LEAD_SECONDS = 40 - 30 = 10
			expect(call.end).toBeLessThanOrEqual(10);
			// Never a range that reaches into or past the playhead.
			expect(call.end).toBeLessThanOrEqual(element.currentTime);
		}
	});

	it("appends bytes=0-initByteEnd exactly once per track, before any fragment", async () => {
		const { fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
		const m = manifest({ initByteEnd: 710 });

		await engine.load({ trackId: "a", versionId: 1, url: m.url, manifest: m });
		await flushUntilQuiescent(() => fetchRange.mock.calls.length);

		const initCalls = fetchRange.mock.calls.filter(([, start, end]) => start === 0 && end === 710);
		expect(initCalls).toHaveLength(1);
		expect(fetchRange.mock.calls[0]).toEqual([m.url, 0, 710]);
	});

	it("settles a loop waiting on timeupdate when teardown() is called, instead of hanging", async () => {
		const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });

		await engine.load(playableTrack("a", 1));
		await flushUntilQuiescent(() => fetchRange.mock.calls.length);

		// The loop should now be blocked on a pending `timeupdate` wait, plus
		// the engine's own trackchange/timeupdate listener.
		expect(element.listenerCount("timeupdate")).toBeGreaterThanOrEqual(2);

		const callsBeforeTeardown = fetchRange.mock.calls.length;
		engine.teardown();
		await flushUntilQuiescent(() => element.listenerCount("timeupdate"));

		// The pending wait's own listener, and the engine's permanent one,
		// must both be gone — proof the wait actually resolved rather than
		// hanging forever.
		expect(element.listenerCount("timeupdate")).toBe(0);
		expect(fetchRange.mock.calls.length).toBe(callsBeforeTeardown);
	});

	it("reports getTrackTime() relative to the current track, not the shared timeline", async () => {
		const { element, engine } = makeEngine({ secondsPerAppend: 10 });

		// Track "a" is exactly 1 second (44100 samples @ 44100Hz).
		await engine.load(playableTrack("a", 1, { sampleCount: 44100, sampleRate: 44100 }));
		await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100, sampleRate: 44100 }));

		element.currentTime = 1.25; // 0.25s into track "b"

		expect(engine.getTrackTime()).toBeCloseTo(0.25, 9);
	});

	it("surfaces a rejected fetchRange as an error event rather than an unhandled rejection", async () => {
		const fetchRange = vi.fn(async () => {
			throw new Error("network down");
		});
		const { engine } = makeEngine({ fetchRange });

		const onError = vi.fn();
		engine.subscribe({ error: onError });

		await engine.load(playableTrack("a", 1));
		await flushUntilQuiescent(() => onError.mock.calls.length);

		expect(onError).toHaveBeenCalledTimes(1);
		expect(onError.mock.calls[0][0]).toBeInstanceOf(Error);
		expect((onError.mock.calls[0][0] as Error).message).toBe("network down");
	});
	describe("seeking", () => {
		it("measures buffered-ahead from the range containing the playhead, not the last range", async () => {
			// Two 100s tracks. Seek into b (buffer grows to [100,180]), then
			// seek back into a hole at 50s. The last range ends 130s ahead of
			// the playhead; the loop must not wait on that stale range.
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			element.seek(145);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			expect(sb.ranges[sb.ranges.length - 1].end - 50).toBeGreaterThan(LEAD_SECONDS);
			const before = fetchRange.mock.calls.length;

			element.seek(50);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls.length).toBeGreaterThan(before);
		});

		it("restarts the loop one fragment before the seek target", async () => {
			const fragments = Array.from({ length: 10 }, (_, i) => ({ start: i * 100, end: i * 100 + 99 }));
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { fragments, sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			fetchRange.mockClear();

			element.seek(65);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			// init, then fragment 5 (starts at 50s, one before the 60s fragment).
			expect(fetchRange.mock.calls[0]).toEqual([expect.any(String), 0, 710]);
			expect(fetchRange.mock.calls[1]).toEqual([expect.any(String), 500, 599]);
		});

		it("refetches evicted media when the user seeks back", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			for (const t of [10, 20, 30, 40, 50, 60, 70]) {
				element.advanceTime(t);
				await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			}
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			expect(sb.removeCalls.length).toBeGreaterThan(0);
			fetchRange.mockClear();

			element.seek(0);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls[0]).toEqual([expect.any(String), 0, 710]);
			expect(fetchRange.mock.calls[1][1]).toBe(1000); // fragment 0
		});

		it("does nothing when the target is already buffered", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const before = fetchRange.mock.calls.length;

			// 15s, not near 0: buffer is [0,40], so only 25s is ahead and a
			// needless rebuild would not be hidden by backpressure.
			element.seek(15);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls.length).toBe(before);
		});

		it("seeks into an appended next track by rebuilding from that track's fragments", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100, fragments: Array.from({ length: 10 }, (_, i) => ({ start: 5000 + i * 100, end: 5000 + i * 100 + 99 })) }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			fetchRange.mockClear();

			element.seek(100 + 45); // 45s into track b
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls[1]).toEqual([expect.any(String), 5300, 5399]); // b, fragment 3
		});
		it("waits for an in-flight append to finish before appending after a seek", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];

			// Old loop appends fragment 3 and that append stays in flight.
			sb.holdUpdates = true;
			element.advanceTime(10);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			expect(sb.updating).toBe(true);
			const appendsBefore = sb.appendCalls.length;

			element.seek(65);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			expect(sb.appendCalls.length).toBe(appendsBefore);

			sb.finishPending();
			await flushUntilQuiescent(() => sb.appendCalls.length);
			expect(sb.appendCalls.length).toBeGreaterThan(appendsBefore);
		});

		it("a loop stopped by a seek does not evict after its pending append resolves", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];

			sb.holdUpdates = true;
			element.advanceTime(10);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const removesBefore = sb.removeCalls.length;

			// Seek to 65: unbuffered, and 65 - LEAD_SECONDS = 35 is past the
			// buffer start, so a stale eviction would remove [0, 35]. The new
			// loop is parked waiting for the held append, so any remove here
			// came from the old loop.
			element.seek(65);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			expect(sb.removeCalls.length).toBe(removesBefore);

			sb.finishPending();
			await flushUntilQuiescent(() => sb.appendCalls.length);
		});

		it("waits for an in-flight append before changing timestampOffset after a cross-track seek", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			const onError = vi.fn();
			engine.subscribe({ error: onError });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];

			sb.holdUpdates = true;
			element.advanceTime(10);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			expect(sb.updating).toBe(true);
			const appendsBefore = sb.appendCalls.length;

			element.seek(145); // into b, offset 100
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			expect(onError).not.toHaveBeenCalled();

			sb.finishPending();
			await flushUntilQuiescent(() => sb.appendCalls.length);
			expect(onError).not.toHaveBeenCalled();
			expect(sb.timestampOffset).toBe(100);
			expect(sb.appendCalls.length).toBeGreaterThan(appendsBefore);
		});

		it("rebuilds when the target sits in a hole just before a buffered range", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			sb.ranges = [
				{ start: 0, end: 40 },
				{ start: 50.1, end: 80 },
			];
			const before = fetchRange.mock.calls.length;

			element.seek(50);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls.length).toBeGreaterThan(before);
		});
	});

	describe("discardNext", () => {
		const B_URL = "/api/stream/b/gapless/alac?version_id=2";
		const C_URL = "/api/stream/c/gapless/alac?version_id=3";

		it("removes the appended track's media and stops appending it", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100, url: B_URL }));
			// Play to 80s so the loop has appended into b (offset 100s).
			for (const t of [10, 20, 30, 40, 50, 60, 70, 80]) {
				element.advanceTime(t);
				await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			}
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			expect(fetchRange.mock.calls.some(([url]) => url === B_URL)).toBe(true);
			expect(sb.ranges.at(-1)!.end).toBeGreaterThan(100);
			fetchRange.mockClear();

			engine.discardNext();
			await flushUntilQuiescent(() => sb.removeCalls.length + fetchRange.mock.calls.length);

			// Not `.at(-1)`: refilling the kept track evicts behind the playhead.
			expect(sb.removeCalls).toContainEqual({ start: 100, end: Number.POSITIVE_INFINITY });
			expect(sb.ranges.at(-1)!.end).toBeLessThanOrEqual(100);
			element.advanceTime(90);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			for (const [url] of fetchRange.mock.calls) {
				expect(url).not.toBe(B_URL);
			}
			expect(engine.getTrackDuration()).toBe(100);
		});

		it("re-appends a different next track after a discard", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			const onError = vi.fn();
			engine.subscribe({ error: onError });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100, url: B_URL }));
			engine.discardNext();
			await engine.prepareNext(playableTrack("c", 3, { sampleCount: 44100 * 100, url: C_URL }));
			for (const t of [10, 20, 30, 40, 50, 60, 70, 80, 90]) {
				element.advanceTime(t);
				await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			}
			const urls = fetchRange.mock.calls.map(([url]) => url);
			expect(urls).toContain(C_URL);
			expect(urls).not.toContain(B_URL);
			expect(onError).not.toHaveBeenCalled();
			element.advanceTime(105);
			expect(engine.getTrackTime()).toBeCloseTo(5, 3);
		});

		it("waits for an in-flight append before removing the discarded media", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100, url: B_URL }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];

			sb.holdUpdates = true;
			element.advanceTime(10);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			expect(sb.updating).toBe(true);
			const removesBefore = sb.removeCalls.length;

			engine.discardNext();
			await flushUntilQuiescent(() => sb.removeCalls.length);
			expect(sb.removeCalls.length).toBe(removesBefore);

			sb.finishPending();
			await flushUntilQuiescent(() => sb.removeCalls.length + sb.appendCalls.length);
			expect(sb.removeCalls.at(-1)).toEqual({ start: 100, end: Number.POSITIVE_INFINITY });
		});

		it("does nothing when no next track is appended", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			const before = fetchRange.mock.calls.length;

			engine.discardNext();
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(sb.removeCalls).toHaveLength(0);
			expect(fetchRange.mock.calls.length).toBe(before);
		});

		it("does nothing before load()", () => {
			const { engine } = makeEngine();
			expect(() => engine.discardNext()).not.toThrow();
		});
	});
});
