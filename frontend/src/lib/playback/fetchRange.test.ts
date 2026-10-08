import { beforeEach, describe, expect, it, vi } from "vitest";

const resolveApiUrl = vi.fn((endpoint: string) => `https://server.example${endpoint}`);
vi.mock("@/api/server", () => ({ resolveApiUrl: (endpoint: string) => resolveApiUrl(endpoint) }));

const getAuthorizationHeader = vi.fn(() => ({ Authorization: "Bearer test-token" }));
vi.mock("@/api/session", () => ({
	getAuthorizationHeader: () => getAuthorizationHeader(),
}));

const { fetchRange } = await import("@/lib/playback/fetchRange");

function okResponse(status: number, body = new ArrayBuffer(4)) {
	return {
		status,
		ok: status >= 200 && status < 300,
		arrayBuffer: vi.fn().mockResolvedValue(body),
	} as unknown as Response;
}

describe("fetchRange", () => {
	beforeEach(() => {
		resolveApiUrl.mockClear();
		getAuthorizationHeader.mockClear();
	});

	it("sends an inclusive Range header with the numbers unmodified", async () => {
		const fetchMock = vi.fn().mockResolvedValue(okResponse(206));
		vi.stubGlobal("fetch", fetchMock);

		await fetchRange("/api/stream/track/gapless/alac", 711, 5000);

		expect(fetchMock).toHaveBeenCalledTimes(1);
		const [, init] = fetchMock.mock.calls[0];
		expect((init.headers as Record<string, string>).Range).toBe("bytes=711-5000");

		vi.unstubAllGlobals();
	});

	it("includes credentials: include and the Authorization header", async () => {
		const fetchMock = vi.fn().mockResolvedValue(okResponse(206));
		vi.stubGlobal("fetch", fetchMock);

		await fetchRange("/api/stream/track/gapless/alac", 0, 100);

		const [, init] = fetchMock.mock.calls[0];
		expect(init.credentials).toBe("include");
		expect((init.headers as Record<string, string>).Authorization).toBe(
			"Bearer test-token",
		);
		expect(getAuthorizationHeader).toHaveBeenCalled();

		vi.unstubAllGlobals();
	});

	it("resolves with the body's ArrayBuffer on 206", async () => {
		const body = new ArrayBuffer(8);
		const fetchMock = vi.fn().mockResolvedValue(okResponse(206, body));
		vi.stubGlobal("fetch", fetchMock);

		await expect(
			fetchRange("/api/stream/track/gapless/alac", 0, 7),
		).resolves.toBe(body);

		vi.unstubAllGlobals();
	});

	it("rejects on 200 (server ignored Range and sent the whole file)", async () => {
		const fetchMock = vi.fn().mockResolvedValue(okResponse(200));
		vi.stubGlobal("fetch", fetchMock);

		await expect(
			fetchRange("/api/stream/track/gapless/alac", 0, 100),
		).rejects.toThrow();

		vi.unstubAllGlobals();
	});

	it("rejects on 416", async () => {
		const fetchMock = vi.fn().mockResolvedValue(okResponse(416));
		vi.stubGlobal("fetch", fetchMock);

		await expect(
			fetchRange("/api/stream/track/gapless/alac", 0, 100),
		).rejects.toThrow();

		vi.unstubAllGlobals();
	});

	it("rejects on 500", async () => {
		const fetchMock = vi.fn().mockResolvedValue(okResponse(500));
		vi.stubGlobal("fetch", fetchMock);

		await expect(
			fetchRange("/api/stream/track/gapless/alac", 0, 100),
		).rejects.toThrow();

		vi.unstubAllGlobals();
	});
});
