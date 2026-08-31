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
