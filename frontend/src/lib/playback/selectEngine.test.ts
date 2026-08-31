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
