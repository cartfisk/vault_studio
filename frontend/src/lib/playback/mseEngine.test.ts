// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";

import { LEAD_SECONDS, createMseEngine } from "@/lib/playback/mseEngine";
import type { FragmentRange, GaplessManifest } from "@/lib/playback/types";

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
}

/** Records appendBuffer/remove calls and fires `updateend` asynchronously,
 *  like a real SourceBuffer. Each appended chunk grows the buffered range by
 *  a fixed, test-controlled number of seconds — the fakes exist to exercise
 *  control flow, not to decode media. */
class FakeSourceBuffer extends EventTarget {
	mode = "";
	timestampOffset = 0;
	appendCalls: ArrayBuffer[] = [];
	removeCalls: Array<{ start: number; end: number }> = [];

	private bufferedStart = 0;
	private bufferedEnd = 0;

	constructor(private secondsPerAppend: number) {
		super();
	}

	get buffered() {
		const start = this.bufferedStart;
		const end = this.bufferedEnd;
		return {
			length: end > start ? 1 : 0,
			start: () => start,
			end: () => end,
		};
	}

	appendBuffer(data: ArrayBuffer): void {
		this.appendCalls.push(data);
		this.bufferedEnd += this.secondsPerAppend;
		queueMicrotask(() => this.dispatchEvent(new Event("updateend")));
	}

	remove(start: number, end: number): void {
		this.removeCalls.push({ start, end });
		this.bufferedStart = end;
		queueMicrotask(() => this.dispatchEvent(new Event("updateend")));
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

/** Flush every currently-pending microtask, including ones chained by
 *  microtasks scheduled while flushing. Node drains the whole microtask
 *  queue before running a macrotask, so one `setTimeout(0)` is enough
 *  regardless of how deep the chain of awaits goes. */
function flush(): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, 0));
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

		await engine.load("a", 1, manifest({ fragments: Array.from({ length: 10 }, (_, i) => ({ start: i, end: i })) }));
		await flush();

		expect(fetchRange.mock.calls.length).toBeGreaterThan(0);
		expect(fetchRange.mock.calls.length).toBeLessThan(11);
	});

	it("evicts buffered data behind currentTime - LEAD_SECONDS, and never ahead of currentTime", async () => {
		const { element, engine } = makeEngine({ secondsPerAppend: 10 });

		await engine.load("a", 1, manifest());
		await flush();

		// currentTime pinned at 0 so far: nothing should have been evicted
		// yet (cutoff would be negative).
		expect(element.listenerCount("timeupdate")).toBeGreaterThan(0);

		element.advanceTime(40);
		await flush();

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

		await engine.load("a", 1, m);
		await flush();

		const initCalls = fetchRange.mock.calls.filter(([, start, end]) => start === 0 && end === 710);
		expect(initCalls).toHaveLength(1);
		expect(fetchRange.mock.calls[0]).toEqual([m.url, 0, 710]);
	});

	it("settles a loop waiting on timeupdate when teardown() is called, instead of hanging", async () => {
		const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });

		await engine.load("a", 1, manifest());
		await flush();

		// The loop should now be blocked on a pending `timeupdate` wait, plus
		// the engine's own trackchange/timeupdate listener.
		expect(element.listenerCount("timeupdate")).toBeGreaterThanOrEqual(2);

		const callsBeforeTeardown = fetchRange.mock.calls.length;
		engine.teardown();
		await flush();

		// The pending wait's own listener, and the engine's permanent one,
		// must both be gone — proof the wait actually resolved rather than
		// hanging forever.
		expect(element.listenerCount("timeupdate")).toBe(0);

		await flush();
		expect(fetchRange.mock.calls.length).toBe(callsBeforeTeardown);
	});

	it("reports getTrackTime() relative to the current track, not the shared timeline", async () => {
		const { element, engine } = makeEngine({ secondsPerAppend: 10 });

		// Track "a" is exactly 1 second (44100 samples @ 44100Hz).
		await engine.load("a", 1, manifest({ sampleCount: 44100, sampleRate: 44100 }));
		await engine.prepareNext("b", 2, manifest({ sampleCount: 44100, sampleRate: 44100 }));

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

		await engine.load("a", 1, manifest());
		await flush();

		expect(onError).toHaveBeenCalledTimes(1);
		expect(onError.mock.calls[0][0]).toBeInstanceOf(Error);
		expect((onError.mock.calls[0][0] as Error).message).toBe("network down");
	});
});
