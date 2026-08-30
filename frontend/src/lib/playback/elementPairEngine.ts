import { SILENT_AUDIO_DATA_URI, normalizeMediaUrl } from "@/lib/gaplessPreload";
import type { GaplessManifest, PlaybackEngine, PlaybackEngineEvents } from "@/lib/playback/types";

/**
 * Two <audio> elements ping-ponging, one per track, swapped at the boundary.
 * Moved out of MusicPlayer.tsx behind the PlaybackEngine interface, verbatim
 * where the original code carried a comment explaining a specific failure it
 * was written to fix.
 *
 * Every track change here is a swap: `canAppend()` always returns false.
 * There is no shared timeline, so `getTrackTime()`/`seekToTrackTime()` are
 * already track-relative with no offset to apply.
 */

export interface ElementPairEngineDeps {
	elA: { current: HTMLAudioElement | null };
	elB: { current: HTMLAudioElement | null };
}

export interface SwapOptions {
	volume: number;
	isPlaying: boolean;
	onDurationKnown: (duration: number) => void;
	onPlayingChange: (isPlaying: boolean) => void;
}

export interface LoadFreshOptions {
	volume: number;
	isPlaying: boolean;
	onPlayError: (error: unknown) => void;
}

/**
 * The element-pair engine, plus the low-level entry points MusicPlayer.tsx
 * calls directly today while it still owns the swap-vs-load decision
 * (`chooseTransition`) and the `activeKey` React state that decision reads.
 * These take the active/standby elements explicitly instead of relying on
 * the engine's own internal notion of "active", so they cannot drift out of
 * sync with MusicPlayer's bookkeeping. The formal PlaybackEngine methods
 * (`load`, `prepareNext`, ...) use the engine's own internal active/standby
 * tracking and are for the orchestrator introduced in a later task.
 */
export interface ElementPairEngine extends PlaybackEngine {
	/**
	 * iOS Safari will not buffer an <audio> element that has never played
	 * inside a user gesture, which would leave the standby empty at every
	 * swap.
	 *
	 * Only the STANDBY is unlocked here, never the active element. The active
	 * one is unlocked for free by the real playback this same gesture is
	 * about to start, and touching it would race that playback: the
	 * `audioUrl` effect assigns the real `src` and calls `load()`, which
	 * rejects this pending `play()` promise, and the rejection handler would
	 * then strip the real source out from under it.
	 *
	 * A srcless element rejects play(), so the standby gets a 10ms silent
	 * data URI first, removed again once the unlock settles.
	 */
	unlockStandby(standby: HTMLAudioElement | null): void;

	/** True once `unlockStandby` has succeeded once this session. */
	isStandbyUnlocked(): boolean;

	/** Buffers the next track into the standby element, the way the preload
	 *  path does at ~20s remaining. */
	prepareStandby(standby: HTMLAudioElement | null, url: string, volume: number): void;

	/**
	 * Tear the outgoing element down BEFORE starting the incoming one.
	 * A half-finished swap plays two tracks at once, which is worse than a gap.
	 */
	swapTo(active: HTMLAudioElement, standby: HTMLAudioElement, opts: SwapOptions): void;

	/** Fallback: the existing load path, unchanged. Returns a cleanup
	 *  function for the effect that invoked it. */
	loadFresh(active: HTMLAudioElement, url: string, opts: LoadFreshOptions): () => void;
}

export function createElementPairEngine(deps: ElementPairEngineDeps): ElementPairEngine {
	const { elA, elB } = deps;

	// Tracks the engine's own notion of "active", used only by the formal
	// PlaybackEngine methods (load/prepareNext/play/pause/...). The low-level
	// methods below (swapTo/loadFresh/prepareStandby/unlockStandby) take
	// active/standby explicitly and never read or write this, so MusicPlayer's
	// own `activeKey` React state cannot drift out of sync with it.
	let activeKey: "a" | "b" = "a";

	const listeners = new Set<PlaybackEngineEvents>();

	function getActive(): HTMLAudioElement | null {
		return activeKey === "a" ? elA.current : elB.current;
	}

	function getStandby(): HTMLAudioElement | null {
		return activeKey === "a" ? elB.current : elA.current;
	}

	function emit<K extends keyof PlaybackEngineEvents>(
		event: K,
		...args: Parameters<NonNullable<PlaybackEngineEvents[K]>>
	) {
		for (const l of listeners) {
			const handler = l[event] as ((...a: unknown[]) => void) | undefined;
			handler?.(...args);
		}
	}

	let unlockedRef = false;
	let unlockInFlightRef = false;

	function unlockStandby(standby: HTMLAudioElement | null): void {
		if (unlockedRef || unlockInFlightRef) return;
		if (!standby) return;

		// Something is already loaded here — leave it alone rather than disturb a
		// preload. A later gesture will retry.
		if (standby.getAttribute("src")) return;

		unlockInFlightRef = true;
		const wasMuted = standby.muted;
		standby.muted = true;
		standby.src = SILENT_AUDIO_DATA_URI;

		const restore = () => {
			standby.muted = wasMuted;
			// Only strip what this routine attached. By the time this runs the
			// element may legitimately hold a real track.
			if (standby.getAttribute("src") === SILENT_AUDIO_DATA_URI) {
				standby.removeAttribute("src");
				standby.load();
			}
			unlockInFlightRef = false;
		};

		standby
			.play()
			.then(() => {
				standby.pause();
				unlockedRef = true;
				restore();
			})
			.catch((error) => {
				// Leave unlockedRef false so a later gesture retries rather than
				// silently losing gapless for the rest of the session.
				console.error("Failed to unlock standby audio element:", error);
				restore();
			});
	}

	function performPrepareStandby(standby: HTMLAudioElement | null, url: string, volume: number): void {
		if (!standby) return;
		if (
			normalizeMediaUrl(standby.src, window.location.href) ===
			normalizeMediaUrl(url, window.location.href)
		)
			return;

		standby.src = url;
		standby.volume = volume;
		standby.load();
	}

	function prepareStandby(standby: HTMLAudioElement | null, url: string, volume: number): void {
		performPrepareStandby(standby, url, volume);
	}

	function performSwap(active: HTMLAudioElement, standby: HTMLAudioElement, opts: SwapOptions): void {
		// Tear the outgoing element down BEFORE starting the incoming one.
		// A half-finished swap plays two tracks at once, which is worse than a gap.
		active.pause();
		active.removeAttribute("src");
		active.load();

		standby.volume = opts.volume;

		// `loadedmetadata` fired on the standby ~20s ago, while it had no
		// listeners bound, and will not fire again for this resource. Without
		// this, `duration` keeps the previous track's value and mistimes the
		// next preload.
		if (Number.isFinite(standby.duration) && standby.duration > 0) {
			opts.onDurationKnown(standby.duration);
		}

		if (opts.isPlaying) {
			standby.play().catch((error) => {
				console.error("Failed to play swapped element:", error);
				// The outgoing element's `pause` was suppressed as stale, so nothing
				// else will correct `isPlaying`. Report the real state.
				opts.onPlayingChange(false);
			});
		}
	}

	function swapTo(active: HTMLAudioElement, standby: HTMLAudioElement, opts: SwapOptions): void {
		performSwap(active, standby, opts);
	}

	function performLoadFresh(active: HTMLAudioElement, url: string, opts: LoadFreshOptions): () => void {
		active.src = url;
		active.volume = opts.volume;
		active.load();

		if (opts.isPlaying) {
			const handleLoadedData = () => {
				active.play().catch((error) => {
					opts.onPlayError(error);
				});
			};

			active.addEventListener("loadeddata", handleLoadedData, { once: true });

			const handleCanPlay = () => {
				if (active.paused && active.readyState >= 2) {
					active.play().catch((error) => {
						opts.onPlayError(error);
					});
				}
			};

			active.addEventListener("canplay", handleCanPlay, { once: true });

			return () => {
				active.removeEventListener("loadeddata", handleLoadedData);
				active.removeEventListener("canplay", handleCanPlay);
			};
		}

		return () => {};
	}

	function loadFresh(active: HTMLAudioElement, url: string, opts: LoadFreshOptions): () => void {
		return performLoadFresh(active, url, opts);
	}

	function canAppend(): boolean {
		// Every track change is a swap; this engine is never gapless.
		return false;
	}

	function getTrackTime(): number {
		return getActive()?.currentTime ?? 0;
	}

	function seekToTrackTime(seconds: number): void {
		const active = getActive();
		if (active) active.currentTime = seconds;
	}

	function getTrackDuration(): number {
		return getActive()?.duration ?? 0;
	}

	function setVolume(v: number): void {
		const active = getActive();
		if (active) active.volume = v;
	}

	async function play(): Promise<void> {
		await getActive()?.play();
	}

	function pause(): void {
		getActive()?.pause();
	}

	async function load(trackId: string, _versionId: number | null, manifest: GaplessManifest): Promise<void> {
		const active = getActive();
		const standby = getStandby();
		if (!active) return;

		const targetUrl = normalizeMediaUrl(manifest.url, window.location.href);
		if (
			standby &&
			normalizeMediaUrl(standby.src, window.location.href) === targetUrl &&
			standby.readyState >= 3
		) {
			performSwap(active, standby, {
				volume: active.volume,
				isPlaying: !active.paused,
				onDurationKnown: () => {},
				onPlayingChange: () => {},
			});
			activeKey = activeKey === "a" ? "b" : "a";
			emit("trackchange", trackId);
			return;
		}

		performLoadFresh(active, manifest.url, {
			volume: active.volume,
			isPlaying: false,
			onPlayError: (error) => emit("error", error),
		});
	}

	async function prepareNext(_trackId: string, _versionId: number | null, manifest: GaplessManifest): Promise<void> {
		performPrepareStandby(getStandby(), manifest.url, getActive()?.volume ?? 1);
	}

	function teardown(): void {
		// Pause and detach BOTH elements. The standby can be holding a fully
		// buffered signed stream for the account that just signed out, so logout
		// has to reach it as well as the active one.
		for (const element of [elA.current, elB.current]) {
			if (!element) continue;
			element.pause();
			element.removeAttribute("src");
			element.load();
		}
		unlockedRef = false;
	}

	function subscribe(events: PlaybackEngineEvents): () => void {
		listeners.add(events);
		return () => listeners.delete(events);
	}

	function isStandbyUnlocked(): boolean {
		return unlockedRef;
	}

	return {
		unlockStandby,
		isStandbyUnlocked,
		prepareStandby,
		swapTo,
		loadFresh,
		canAppend,
		getTrackTime,
		seekToTrackTime,
		getTrackDuration,
		setVolume,
		play,
		pause,
		load,
		prepareNext,
		teardown,
		subscribe,
	};
}
