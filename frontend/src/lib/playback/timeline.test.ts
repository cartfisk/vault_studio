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
