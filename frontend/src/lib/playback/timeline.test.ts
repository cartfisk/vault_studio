import { describe, expect, it } from "vitest";

import {
	durationSeconds,
	elementTimeFor,
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
		expect(t.offsetSeconds).toBe(0);
		expect(t.durationSamples).toBe(44100);
	});

	it("places each track at the running sum, not duration * index", () => {
		const placed: PlacedTrack[] = [];
		placed.push(placeTrack(placed, "a", 1, manifest(44100)));       // 1.0s
		placed.push(placeTrack(placed, "b", 2, manifest(66150)));       // 1.5s
		placed.push(placeTrack(placed, "c", 3, manifest(22050)));       // 0.5s

		expect(placed[0].offsetSeconds).toBeCloseTo(0, 9);
		expect(placed[1].offsetSeconds).toBeCloseTo(1.0, 9);
		expect(placed[2].offsetSeconds).toBeCloseTo(2.5, 9);
	});

	it("places tracks at different sample rates using the running sum of SECONDS, not raw sample counts", () => {
		// A real lossless library mixes 44.1kHz and 48kHz masters routinely.
		// Sample counts from different rates are not the same unit and cannot
		// be summed directly.
		const placed: PlacedTrack[] = [];
		placed.push(placeTrack(placed, "a", 1, manifest(44100, 44100)));  // 1.000s @ 44.1k
		placed.push(placeTrack(placed, "b", 2, manifest(96000, 48000)));  // 2.000s @ 48k
		placed.push(placeTrack(placed, "c", 3, manifest(44100, 44100)));  // 1.000s @ 44.1k

		expect(placed[0].offsetSeconds).toBeCloseTo(0, 9);
		expect(placed[1].offsetSeconds).toBeCloseTo(1.0, 9);
		expect(placed[2].offsetSeconds).toBeCloseTo(3.0, 9);
	});

	it("does not accumulate float error across a long queue", () => {
		// A duration that is not exactly representable as a float fraction.
		const placed: PlacedTrack[] = [];
		for (let i = 0; i < 500; i++) {
			placed.push(placeTrack(placed, `t${i}`, i, manifest(44099)));
		}
		// Summing per-track seconds must stay accurate to well under a sample
		// period, however long the queue.
		expect(placed[499].offsetSeconds).toBeCloseTo((44099 * 499) / 44100, 9);
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

	it("resolves the right track and track-relative time across a mixed-rate boundary", () => {
		const mixed: PlacedTrack[] = [];
		mixed.push(placeTrack(mixed, "a", 1, manifest(44100, 44100)));  // 1.000s @ 44.1k
		mixed.push(placeTrack(mixed, "b", 2, manifest(96000, 48000)));  // 2.000s @ 48k

		const justBeforeBoundary = trackTimeFor(mixed, 0.999);
		expect(justBeforeBoundary?.track.trackId).toBe("a");
		expect(justBeforeBoundary?.trackTime).toBeCloseTo(0.999, 9);

		const atBoundary = trackTimeFor(mixed, 1.0);
		expect(atBoundary?.track.trackId).toBe("b");
		expect(atBoundary?.trackTime).toBeCloseTo(0, 9);

		const insideSecond = trackTimeFor(mixed, 2.5);
		expect(insideSecond?.track.trackId).toBe("b");
		expect(insideSecond?.trackTime).toBeCloseTo(1.5, 9);
	});

	it("tolerates a currentTime a hair past a track's mathematical end (one sample period of slack)", () => {
		const singleTrack: PlacedTrack[] = [placeTrack([], "a", 1, manifest(44100, 44100))];
		const duration = durationSeconds(singleTrack[0]);
		const hairPast = duration + 1 / singleTrack[0].sampleRate / 2;

		const got = trackTimeFor(singleTrack, hairPast);
		expect(got?.track.trackId).toBe("a");
		expect(got?.trackTime).toBeCloseTo(hairPast, 9);
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

describe("durationSeconds", () => {
	it("converts samples to seconds at the track's own rate", () => {
		const placed: PlacedTrack[] = [];
		placed.push(placeTrack(placed, "a", 1, manifest(48000, 48000)));
		placed.push(placeTrack(placed, "b", 2, manifest(96000, 48000)));

		expect(placed[1].offsetSeconds).toBeCloseTo(1.0, 9);
		expect(durationSeconds(placed[1])).toBeCloseTo(2.0, 9);
	});
});
