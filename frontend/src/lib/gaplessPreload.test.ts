import { describe, expect, it } from "vitest";

import {
	chooseTransition,
	isPreloadStale,
	normalizeMediaUrl,
	PRELOAD_LEAD_SECONDS,
	SIGNED_URL_TTL_SECONDS,
	SWAP_MIN_READY_STATE,
	shouldStartPreload,
} from "@/lib/gaplessPreload";

describe("shouldStartPreload", () => {
	it("fires once inside the lead window", () => {
		expect(shouldStartPreload({ currentTime: 100, duration: 240 })).toBe(false);
		expect(shouldStartPreload({ currentTime: 219, duration: 240 })).toBe(false);
		expect(shouldStartPreload({ currentTime: 220, duration: 240 })).toBe(true);
		expect(shouldStartPreload({ currentTime: 239, duration: 240 })).toBe(true);
	});

	it("fires immediately for tracks shorter than the lead window", () => {
		expect(shouldStartPreload({ currentTime: 0, duration: 12 })).toBe(true);
	});

	it("fires on a forward seek into the window", () => {
		expect(shouldStartPreload({ currentTime: 235, duration: 240 })).toBe(true);
	});

	it("stops firing after a backward seek out of the window", () => {
		expect(shouldStartPreload({ currentTime: 30, duration: 240 })).toBe(false);
	});

	it("refuses to decide without a usable duration", () => {
		expect(shouldStartPreload({ currentTime: 5, duration: 0 })).toBe(false);
		expect(shouldStartPreload({ currentTime: 5, duration: Number.NaN })).toBe(
			false,
		);
		expect(shouldStartPreload({ currentTime: 5, duration: Infinity })).toBe(
			false,
		);
	});

	it("refuses to decide without a usable currentTime", () => {
		expect(shouldStartPreload({ currentTime: Number.NaN, duration: 240 })).toBe(
			false,
		);
		expect(shouldStartPreload({ currentTime: -1, duration: 240 })).toBe(false);
	});

	it("honours a custom lead window", () => {
		expect(
			shouldStartPreload({ currentTime: 200, duration: 240, leadSeconds: 60 }),
		).toBe(true);
		expect(
			shouldStartPreload({ currentTime: 100, duration: 240, leadSeconds: 60 }),
		).toBe(false);
	});

	// Pins the tuning value the rest of this suite's expectations are computed
	// from. If someone retunes the lead window, these expectations must be
	// recomputed deliberately rather than silently drifting.
	it("defaults the lead window to 20 seconds", () => {
		expect(PRELOAD_LEAD_SECONDS).toBe(20);
	});
});

describe("isPreloadStale", () => {
	const signedAt = 1_000_000;

	it("treats a freshly signed url as usable", () => {
		expect(isPreloadStale({ signedAt, now: signedAt + 5_000 })).toBe(false);
	});

	it("treats a url past its ttl as stale", () => {
		expect(isPreloadStale({ signedAt, now: signedAt + 301_000 })).toBe(true);
	});

	it("treats a url inside the safety margin as stale before it actually expires", () => {
		expect(isPreloadStale({ signedAt, now: signedAt + 275_000 })).toBe(true);
		expect(isPreloadStale({ signedAt, now: signedAt + 265_000 })).toBe(false);
	});

	// This constant duplicates the server's SIGNED_URL_TTL across the
	// client/server boundary. Pinning it means a silent edit on either side
	// breaks a test instead of producing 403s at swap time in production.
	it("mirrors the server ttl", () => {
		expect(SIGNED_URL_TTL_SECONDS).toBe(300);
	});
});

describe("chooseTransition", () => {
	const targetUrl = "https://example.test/stream/abc";

	it("swaps when the standby holds the target and is buffered enough", () => {
		expect(
			chooseTransition({
				standbySrc: targetUrl,
				targetUrl,
				readyState: SWAP_MIN_READY_STATE,
			}),
		).toBe("swap");
		expect(
			chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: 4 }),
		).toBe("swap");
	});

	it("loads when the standby is empty", () => {
		expect(
			chooseTransition({ standbySrc: null, targetUrl, readyState: 4 }),
		).toBe("load");
		expect(chooseTransition({ standbySrc: "", targetUrl, readyState: 4 })).toBe(
			"load",
		);
	});

	it("loads when the standby holds a different url", () => {
		expect(
			chooseTransition({
				standbySrc: "https://example.test/stream/other",
				targetUrl,
				readyState: 4,
			}),
		).toBe("load");
	});

	it("loads when the standby has not buffered enough", () => {
		expect(
			chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: 2 }),
		).toBe("load");
		expect(
			chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: 0 }),
		).toBe("load");
	});
});

describe("normalizeMediaUrl", () => {
	const base = "https://vault.example.test/library";

	it("resolves a relative api path against the page base", () => {
		expect(normalizeMediaUrl("/api/media/stream/abc?expires=1", base)).toBe(
			"https://vault.example.test/api/media/stream/abc?expires=1",
		);
	});

	it("leaves an already-absolute url untouched", () => {
		const absolute = "https://cdn.example.test/stream/abc";
		expect(normalizeMediaUrl(absolute, base)).toBe(absolute);
	});

	it("makes a relative url and its resolved form compare equal", () => {
		const relative = "/api/media/stream/abc";
		const resolved = "https://vault.example.test/api/media/stream/abc";
		expect(normalizeMediaUrl(relative, base)).toBe(
			normalizeMediaUrl(resolved, base),
		);
	});

	it("returns the input unchanged when it cannot be parsed", () => {
		expect(normalizeMediaUrl("::not a url::", "::also bad::")).toBe(
			"::not a url::",
		);
	});
});
