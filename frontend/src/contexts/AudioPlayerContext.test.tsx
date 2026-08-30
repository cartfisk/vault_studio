// @vitest-environment jsdom

/**
 * Covers the three things in AudioPlayerContext that a browser is not needed
 * to verify: that transport time is read TRACK-relative, that the preload
 * trigger appends only when both the selection and the live engine agree, and
 * that a browser with no lossless codec support sends no `codecs` parameter.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const codecSupport = vi.hoisted(() => ({
	codecs: null as string | null,
	supported: [] as string[],
}));

const apis = vi.hoisted(() => ({
	getStreamUrl: vi.fn(),
}));

vi.mock("../lib/playback/codecSupport", () => ({
	codecsParam: () => codecSupport.codecs,
	supportedLosslessCodecs: () => codecSupport.supported,
}));

vi.mock("../api/media", () => ({
	getStreamUrl: apis.getStreamUrl,
	resolveApiMediaUrl: (url?: string | null) => url ?? undefined,
}));

vi.mock("../api/server", () => ({
	resolveApiUrl: (path: string) => path,
}));

vi.mock("../api/tracks", () => ({
	getTrack: vi.fn(async (id: string) => ({ id, waveform: null })),
}));

vi.mock("../api/versions", () => ({
	getVersions: vi.fn(async () => []),
}));

vi.mock("../hooks/useProjectCoverImage", () => ({
	preloadCover: vi.fn(),
}));

vi.mock("../lib/nativeMediaSession", () => ({
	hasNativeMediaSession: false,
	NativeMediaSession: {
		setMetadata: vi.fn(async () => {}),
		setPlaybackState: vi.fn(async () => {}),
		setPositionState: vi.fn(async () => {}),
		addListener: vi.fn(async () => ({ remove: async () => {} })),
	},
}));

vi.mock("./AuthContext", () => ({
	useAuth: () => ({ isAuthenticated: true }),
}));

vi.mock("./PreferencesContext", () => ({
	usePreferences: () => ({ preferences: { default_quality: "lossless" } }),
}));

import { AudioPlayerProvider, useAudioPlayer } from "./AudioPlayerContext";

type Ctx = ReturnType<typeof useAudioPlayer>;

let ctx: Ctx;

function Probe() {
	ctx = useAudioPlayer();
	return null;
}

const MANIFEST = {
	codec: "alac" as const,
	url: "/api/stream/x/gapless/alac?version_id=1",
	sampleRate: 44100,
	sampleCount: 4410000,
	channels: 2,
	initByteEnd: 1000,
	fragments: [{ start: 1001, end: 2000 }],
};

/** A fake engine standing in for the facade MusicPlayer publishes. */
function fakeEngine(trackTime: number, canAppend = true) {
	return {
		// Present so the context recognises this as engine-shaped.
		getTrackTime: vi.fn(() => trackTime),
		getTrackDuration: vi.fn(() => 300),
		getPlaybackRate: vi.fn(() => 1),
		seekToTrackTime: vi.fn(),
		play: vi.fn(async () => {}),
		pause: vi.fn(),
		canAppend: vi.fn(() => canAppend),
		prepareNext: vi.fn(async () => {}),
		teardown: vi.fn(),
	};
}

const TRACKS = [
	{ id: "t1", title: "One", versionId: null },
	{ id: "t2", title: "Two", versionId: null },
	{ id: "t3", title: "Three", versionId: null },
];

function mount() {
	return render(
		<AudioPlayerProvider>
			<Probe />
		</AudioPlayerProvider>,
	);
}

beforeEach(() => {
	localStorage.clear();
	codecSupport.codecs = null;
	codecSupport.supported = [];
	apis.getStreamUrl.mockReset();
	apis.getStreamUrl.mockResolvedValue({ url: "/api/stream/x" });
});

afterEach(() => {
	cleanup();
	vi.useRealTimers();
});

describe("previousTrack time source", () => {
	it("goes to the previous track when the engine reports 2s INTO the track", async () => {
		mount();
		const engine = fakeEngine(2);

		await act(async () => {
			// Third track of the project: under MSE its element time would be
			// ~600s even though only 2s of the track have played.
			await ctx.play(TRACKS[2], TRACKS, false);
		});
		act(() => {
			ctx.audioPlayerRef.current = engine;
		});

		await act(async () => {
			ctx.previousTrack();
		});

		expect(ctx.currentTrack?.id).toBe("t2");
		expect(engine.seekToTrackTime).not.toHaveBeenCalled();
	});

	/**
	 * The discriminator. Same instant of playback, but read as a
	 * timeline-absolute value the way the element's `currentTime` reports it
	 * under MSE. If `previousTrack` were routed through that value it would
	 * take this branch on every track after the first and the back button
	 * would never leave the current track.
	 */
	it("restarts the track when the reported time is past the 3s threshold", async () => {
		mount();
		const engine = fakeEngine(602);

		await act(async () => {
			await ctx.play(TRACKS[2], TRACKS, false);
		});
		act(() => {
			ctx.audioPlayerRef.current = engine;
		});

		await act(async () => {
			ctx.previousTrack();
		});

		expect(ctx.currentTrack?.id).toBe("t3");
		expect(engine.seekToTrackTime).toHaveBeenCalledWith(0);
	});
});

describe("preload trigger engine selection", () => {
	async function runPreload(opts: {
		gapless: boolean;
		canAppend: boolean;
	}) {
		codecSupport.codecs = "alac";
		codecSupport.supported = ["alac"];
		apis.getStreamUrl.mockResolvedValue(
			opts.gapless
				? { url: "/api/stream/x", gapless: MANIFEST }
				: { url: "/api/stream/x" },
		);

		mount();
		const engine = fakeEngine(5, opts.canAppend);

		await act(async () => {
			await ctx.play(TRACKS[0], TRACKS, true);
		});
		act(() => {
			ctx.audioPlayerRef.current = engine;
		});

		// Drive the context into the preload window.
		await act(async () => {
			ctx.onDurationChange(100);
			ctx.onProgressUpdate(95);
		});
		// Let the preload effect's async body settle.
		await act(async () => {
			await Promise.resolve();
			await Promise.resolve();
		});

		return engine;
	}

	it("appends when the current and next tracks are both MSE", async () => {
		const engine = await runPreload({ gapless: true, canAppend: true });

		expect(engine.canAppend).toHaveBeenCalled();
		expect(engine.prepareNext).toHaveBeenCalledWith(
			expect.objectContaining({ trackId: "t2", manifest: MANIFEST }),
		);
		expect(ctx.nextTrackPreload?.engine).toBe("mse");
	});

	it("hands off instead of appending when there is no manifest", async () => {
		const engine = await runPreload({ gapless: false, canAppend: true });

		expect(engine.prepareNext).not.toHaveBeenCalled();
		expect(ctx.nextTrackPreload?.engine).toBe("elementPair");
		// The handoff still publishes the preload the element pair buffers.
		expect(ctx.nextTrackPreload?.trackId).toBe("t2");
	});

	it("hands off when the live engine refuses the track", async () => {
		const engine = await runPreload({ gapless: true, canAppend: false });

		expect(engine.canAppend).toHaveBeenCalled();
		expect(engine.prepareNext).not.toHaveBeenCalled();
		expect(ctx.nextTrackPreload?.trackId).toBe("t2");
	});
});

describe("codecs parameter", () => {
	it("sends no codecs key when the browser supports no lossless codec", async () => {
		codecSupport.codecs = null;
		mount();

		await act(async () => {
			await ctx.play(TRACKS[0], TRACKS, false);
		});

		expect(apis.getStreamUrl).toHaveBeenCalled();
		const params = apis.getStreamUrl.mock.calls[0][1];
		expect("codecs" in params).toBe(false);
	});

	it("sends the codecs list when the browser supports one", async () => {
		codecSupport.codecs = "alac,flac";
		mount();

		await act(async () => {
			await ctx.play(TRACKS[0], TRACKS, false);
		});

		const params = apis.getStreamUrl.mock.calls[0][1];
		expect(params.codecs).toBe("alac,flac");
	});
});
