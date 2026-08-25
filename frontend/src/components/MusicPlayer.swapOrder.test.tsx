// @vitest-environment jsdom

/**
 * Pins the teardown-before-play ordering in the swap branch of MusicPlayer's
 * `audioUrl` effect.
 *
 * Two tracks audible at once is worse than any gap, so the outgoing element
 * must be paused and detached BEFORE the incoming one is told to play. This
 * test drives a real swap through the real component against real jsdom
 * <audio> elements; only the audio-player context and unrelated peripheral
 * hooks are stubbed.
 */

import { act, cleanup, render } from "@testing-library/react";
import { useEffect, useReducer } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const TRACK_A_URL = "https://cdn.test/stream/track-a.mp3";
const TRACK_B_URL = "https://cdn.test/stream/track-b.mp3";

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

/** Ordered log of media operations, tagged with which element they hit. */
let calls: string[] = [];
let labelFor: (el: HTMLMediaElement) => string = () => "?";

const originalPlay = HTMLMediaElement.prototype.play;
const originalPause = HTMLMediaElement.prototype.pause;
const originalLoad = HTMLMediaElement.prototype.load;

function baseState() {
	return {
		currentTrack: { id: "t1", title: "Track A", versionId: null },
		audioUrl: TRACK_A_URL,
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
		onProgressUpdate: vi.fn(),
		onEnded: vi.fn(),
		audioPlayerRef: { current: null },
		shareToken: null,
		sharePassword: null,
	};
}

describe("MusicPlayer swap ordering", () => {
	beforeEach(() => {
		calls = [];
		if (!window.matchMedia) {
			window.matchMedia = ((query: string) => ({
				matches: false,
				media: query,
				addEventListener: () => {},
				removeEventListener: () => {},
			})) as unknown as typeof window.matchMedia;
		}
		HTMLMediaElement.prototype.play = function play(this: HTMLMediaElement) {
			calls.push(`${labelFor(this)}:play`);
			return Promise.resolve();
		};
		HTMLMediaElement.prototype.pause = function pause(this: HTMLMediaElement) {
			calls.push(`${labelFor(this)}:pause`);
		};
		HTMLMediaElement.prototype.load = function load(this: HTMLMediaElement) {
			calls.push(`${labelFor(this)}:load`);
		};
		store.set(baseState());
	});

	afterEach(() => {
		cleanup();
		HTMLMediaElement.prototype.play = originalPlay;
		HTMLMediaElement.prototype.pause = originalPause;
		HTMLMediaElement.prototype.load = originalLoad;
	});

	it("tears the outgoing element down before playing the incoming one", async () => {
		const { container } = render(<MusicPlayer hideControls />);

		const [elA, elB] = Array.from(
			container.querySelectorAll("audio"),
		) as HTMLAudioElement[];
		expect(elA).toBeDefined();
		expect(elB).toBeDefined();

		labelFor = (el) => (el === elA ? "a" : el === elB ? "b" : "?");

		for (const [label, el] of [
			["a", elA],
			["b", elB],
		] as const) {
			const original = el.removeAttribute.bind(el);
			el.removeAttribute = (name: string) => {
				calls.push(`${label}:removeAttribute(${name})`);
				original(name);
			};
		}

		// Buffer the next track into the standby element, the way the preload
		// path does at ~20s remaining.
		await act(async () => {
			store.set({
				nextTrackPreload: {
					trackId: "t2",
					versionId: null,
					url: TRACK_B_URL,
					signedAt: Date.now(),
				},
			});
		});
		expect(elB.src).toBe(TRACK_B_URL);

		// jsdom never actually buffers, so report the readiness the swap needs.
		Object.defineProperty(elB, "readyState", {
			value: 3,
			configurable: true,
		});

		calls = [];

		// The track ended and the context published the next track's URL.
		await act(async () => {
			store.set({
				currentTrack: { id: "t2", title: "Track B", versionId: null },
				audioUrl: TRACK_B_URL,
			});
		});

		expect(calls.slice(0, 4)).toEqual([
			"a:pause",
			"a:removeAttribute(src)",
			"a:load",
			"b:play",
		]);
		// The incoming element must not have been told to play any earlier.
		expect(calls.indexOf("b:play")).toBe(3);
	});
});
