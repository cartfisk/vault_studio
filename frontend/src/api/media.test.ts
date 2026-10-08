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
