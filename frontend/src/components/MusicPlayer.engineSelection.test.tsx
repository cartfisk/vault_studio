// @vitest-environment jsdom

/**
 * Covers the three parts of MSE activation a browser is not needed to
 * verify: that the MSE engine is constructed and loaded only when the server
 * offered a manifest this browser can decode, that the UI reads TRACK-relative
 * time from that engine rather than the element's timeline-absolute value, and
 * that a preload bound for the MSE engine never also buffers the element-pair
 * standby.
 */

import { act, cleanup, render } from "@testing-library/react";
import { useEffect, useReducer } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const TRACK_A_URL = "https://cdn.test/stream/track-a.m4a";
const TRACK_B_URL = "https://cdn.test/stream/track-b.m4a";

const codecSupport = vi.hoisted(() => ({ supported: [] as string[] }));

const mse = vi.hoisted(() => {
	const engine = {
		load: vi.fn(async () => {}),
		play: vi.fn(async () => {}),
		pause: vi.fn(),
		seekToTrackTime: vi.fn(),
		getTrackTime: vi.fn(() => 0),
		getTrackDuration: vi.fn(() => 0),
		setVolume: vi.fn(),
		canAppend: vi.fn(() => true),
		prepareNext: vi.fn(async () => {}),
		teardown: vi.fn(),
		subscribe: vi.fn((_events: unknown) => () => {}),
	};
	return { engine, create: vi.fn(() => engine) };
});

const store = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	const state: { value: Record<string, unknown> } = { value: {} };
	return {
		listeners,
		state,
		set(patch: Record<string, unknown>) {
			state.value = { ...state.value, ...patch };
			for (const listener of listeners) listener();
		},
	};
});

vi.mock("../lib/playback/mseEngine", () => ({ createMseEngine: mse.create }));
vi.mock("../lib/playback/fetchRange", () => ({ fetchRange: vi.fn() }));
vi.mock("../lib/playback/codecSupport", () => ({
	supportedLosslessCodecs: () => codecSupport.supported,
}));

vi.mock("../contexts/AudioPlayerContext", () => ({
	useAudioPlayer: () => {
		const [, force] = useReducer((n: number) => n + 1, 0);
		useEffect(() => {
			store.listeners.add(force);
			return () => {
				store.listeners.delete(force);
			};
		}, []);
		return store.state.value;
	},
}));

vi.mock("../contexts/PreferencesContext", () => ({
	usePreferences: () => ({ preferences: { comments_enabled: false } }),
}));

vi.mock("../hooks/useProjectCoverImage", () => ({
	useProjectCoverImage: () => ({ imageUrl: null }),
}));

vi.mock("@tanstack/react-router", () => ({
	useNavigate: () => vi.fn(),
	useRouterState: () => ({ location: { pathname: "/" } }),
}));

vi.mock("web-haptics/react", () => ({
	useWebHaptics: () => vi.fn(),
}));

import MusicPlayer from "./MusicPlayer";

const MANIFEST = {
	codec: "alac" as const,
	url: "/api/media/stream/t1/gapless/alac?version_id=1",
	sampleRate: 44100,
	sampleCount: 4410000,
	channels: 2,
	initByteEnd: 1000,
	fragments: [{ start: 1001, end: 2000 }],
};

const onProgressUpdate = vi.fn();

function baseState() {
	return {
		currentTrack: { id: "t1", title: "Track A", versionId: null },
		audioUrl: TRACK_A_URL,
		currentPlayable: {
			trackId: "t1",
			versionId: null,
			url: TRACK_A_URL,
			manifest: null,
		},
		nextTrackPreload: null,
		isPlaying: true,
		pause: vi.fn(),
		resume: vi.fn(),
		nextTrack: vi.fn(),
		previousTrack: vi.fn(),
		playFromQueue: vi.fn(),
		loopMode: "off" as const,
		toggleLoop: vi.fn(),
		isShuffled: false,
		toggleShuffle: vi.fn(),
		queue: [],
		onPlayingChange: vi.fn(),
		onDurationChange: vi.fn(),
		onProgressUpdate,
		onEnded: vi.fn(),
		audioPlayerRef: { current: null },
		shareToken: null,
		sharePassword: null,
	};
}

const originalPlay = HTMLMediaElement.prototype.play;
const originalLoad = HTMLMediaElement.prototype.load;

function audioElements(container: HTMLElement) {
	const [elA, elB, elMse] = Array.from(
		container.querySelectorAll("audio"),
	) as HTMLAudioElement[];
	return { elA, elB, elMse };
}

describe("MusicPlayer engine selection", () => {
	beforeEach(() => {
		vi.clearAllMocks();
		codecSupport.supported = [];
		mse.engine.getTrackTime.mockReturnValue(0);
		mse.engine.getTrackDuration.mockReturnValue(0);
		(window as unknown as { MediaSource?: unknown }).MediaSource =
			class FakeMediaSource {};
		if (!window.matchMedia) {
			window.matchMedia = ((query: string) => ({
				matches: false,
				media: query,
				addEventListener: () => {},
				removeEventListener: () => {},
			})) as unknown as typeof window.matchMedia;
		}
		HTMLMediaElement.prototype.play = () => Promise.resolve();
		HTMLMediaElement.prototype.load = () => {};
		store.set(baseState());
	});

	afterEach(() => {
		cleanup();
		HTMLMediaElement.prototype.play = originalPlay;
		HTMLMediaElement.prototype.load = originalLoad;
	});

	it("constructs and loads the MSE engine for a supported lossless track", async () => {
		codecSupport.supported = ["alac"];
		store.set({
			currentPlayable: {
				trackId: "t1",
				versionId: null,
				url: TRACK_A_URL,
				manifest: MANIFEST,
			},
		});

		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});

		expect(mse.create).toHaveBeenCalledTimes(1);
		expect(mse.engine.load).toHaveBeenCalledWith(
			expect.objectContaining({ trackId: "t1", manifest: MANIFEST }),
		);
		// The element pair must be left alone: a second live source is worse
		// than any gap.
		const { elA, elB } = audioElements(container);
		expect(elA.getAttribute("src")).toBeNull();
		expect(elB.getAttribute("src")).toBeNull();
	});

	it("uses the element pair when there is no manifest", async () => {
		codecSupport.supported = ["alac"];
		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});

		expect(mse.create).not.toHaveBeenCalled();
		expect(audioElements(container).elA.src).toBe(TRACK_A_URL);
	});

	it("uses the element pair when the browser cannot decode the offered codec", async () => {
		codecSupport.supported = [];
		store.set({
			currentPlayable: {
				trackId: "t1",
				versionId: null,
				url: TRACK_A_URL,
				manifest: MANIFEST,
			},
		});

		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});

		expect(mse.create).not.toHaveBeenCalled();
		expect(audioElements(container).elA.src).toBe(TRACK_A_URL);
	});

	/**
	 * The discriminating case: the MSE element's own `currentTime` is an
	 * offset into a shared timeline. If any transport read still goes to the
	 * element, the scrubber jumps to the offset of every track after the
	 * first.
	 */
	it("reports the engine's track-relative time, not the element's timeline offset", async () => {
		codecSupport.supported = ["alac"];
		mse.engine.getTrackDuration.mockReturnValue(200);
		store.set({
			currentPlayable: {
				trackId: "t1",
				versionId: null,
				url: TRACK_A_URL,
				manifest: MANIFEST,
			},
		});

		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});

		const { elMse } = audioElements(container);
		// The element sits 125s into the shared timeline: 120s of a previous
		// track plus the 5s the engine reports for this one.
		Object.defineProperty(elMse, "currentTime", {
			value: 125,
			configurable: true,
		});
		Object.defineProperty(elMse, "duration", {
			value: 320,
			configurable: true,
		});

		// Only now does the engine report a non-zero track time, so anything
		// the UI publishes from here on came from one source or the other.
		mse.engine.getTrackTime.mockReturnValue(5);
		await act(async () => {
			elMse.dispatchEvent(new Event("timeupdate"));
		});

		const reported = onProgressUpdate.mock.calls.map((c) => c[0]);
		expect(reported).toContain(5);
		expect(reported).not.toContain(125);
	});

	/**
	 * At a gapless boundary no `ended` fires — that is the point — so
	 * `trackchange` is the only signal that the previous track finished. It
	 * has to drive the same advance `ended` does, or the in-app UI and the
	 * lock screen both keep showing the track that already finished.
	 */
	it("advances the app on the engine's trackchange, with no ended event", async () => {
		codecSupport.supported = ["alac"];
		const onEnded = vi.fn();
		store.set({
			onEnded,
			currentPlayable: {
				trackId: "t1",
				versionId: null,
				url: TRACK_A_URL,
				manifest: MANIFEST,
			},
		});

		render(<MusicPlayer hideControls />);
		await act(async () => {});

		expect(mse.engine.subscribe).toHaveBeenCalledTimes(1);
		const events = mse.engine.subscribe.mock.calls[0][0] as {
			trackchange?: (trackId: string) => void;
		};
		expect(onEnded).not.toHaveBeenCalled();

		await act(async () => {
			events.trackchange?.("t2");
		});
		expect(onEnded).toHaveBeenCalledTimes(1);
	});

	it("does not buffer the element-pair standby for an MSE-bound preload", async () => {
		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});

		const { elB } = audioElements(container);

		await act(async () => {
			store.set({
				nextTrackPreload: {
					trackId: "t2",
					versionId: null,
					url: TRACK_B_URL,
					signedAt: Date.now(),
					engine: "mse",
				},
			});
		});
		expect(elB.getAttribute("src")).toBeNull();

		// ...but an element-pair preload still buffers, exactly as before.
		await act(async () => {
			store.set({
				nextTrackPreload: {
					trackId: "t2",
					versionId: null,
					url: TRACK_B_URL,
					signedAt: Date.now(),
					engine: "elementPair",
				},
			});
		});
		expect(elB.src).toBe(TRACK_B_URL);
	});
});
