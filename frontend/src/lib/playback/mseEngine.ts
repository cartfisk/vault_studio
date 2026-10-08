import { LOSSLESS_MIME } from "@/lib/playback/codecSupport";
import { durationSeconds, elementTimeFor, placeTrack, trackTimeFor } from "@/lib/playback/timeline";
import type {
	FragmentRange,
	GaplessManifest,
	PlacedTrack,
	PlayableTrack,
	PlaybackEngine,
	PlaybackEngineEvents,
} from "@/lib/playback/types";

/**
 * Only append while the buffer is within this many seconds of the playhead;
 * evict beyond it behind the playhead.
 *
 * Not a tuning preference: a spike on a real iPhone raced to append a 37MB
 * file and died with QuotaExceededError at 15MB. Appending faster than
 * playback is the failure mode this bounds.
 */
export const LEAD_SECONDS = 30;

/** Backend fragment length (`FragmentDurationMicros`). Fragments start on a
 *  packet boundary, so fragment k begins at k*FRAGMENT_SECONDS plus up to
 *  one codec frame; seeks start one fragment early to cover that. */
export const FRAGMENT_SECONDS = 10;

export interface MseEngineDeps {
	element: HTMLAudioElement;
	mediaSourceImpl: typeof MediaSource;
	fetchRange(url: string, start: number, end: number): Promise<ArrayBuffer>;
}

interface AppendJob {
	trackId: string;
	offsetSeconds: number;
	url: string;
	initByteEnd: number;
	fragments: FragmentRange[];
	initAppended: boolean;
	fragIndex: number;
}

/** Minimal shape we need from a SourceBuffer, so fakes don't have to
 *  implement the entire (huge) real interface. */
interface MinimalSourceBuffer extends EventTarget {
	mode: string;
	timestampOffset: number;
	updating: boolean;
	buffered: { length: number; start(i: number): number; end(i: number): number };
	appendBuffer(data: ArrayBuffer): void;
	remove(start: number, end: number): void;
}

export function createMseEngine(deps: MseEngineDeps): PlaybackEngine {
	const { element, mediaSourceImpl, fetchRange } = deps;

	let mediaSource: MediaSource | null = null;
	let sourceBuffer: MinimalSourceBuffer | null = null;
	let sourceBufferMime: string | null = null;

	let placed: PlacedTrack[] = [];
	let jobs: AppendJob[] = [];
	let lastTrackId: string | null = null;
	/** Manifests are kept per placed track so a seek can rebuild jobs. */
	const manifests = new Map<string, GaplessManifest>();
	/** Set by discardNext(): everything from here on is a retracted track's
	*  media. The loop removes it before its next append, so the remove and
	*  the appends have one writer and cannot interleave. */
	let trimFrom: number | null = null;

	const listeners = new Set<PlaybackEngineEvents>();

	/**
	 * Each load() starts a new session with its own stop token, instead of
	 * one shared mutable `stopped` flag. A shared flag would race: teardown()
	 * (or a second load()) sets it true then a fresh load() immediately sets
	 * it false again for the new session, and a stale loop's pending
	 * `waitFor` — resolved by that same flip — would read the flag AFTER
	 * it flipped back and wrongly conclude it should keep running. A token
	 * per session can't un-stop once stopped, so a stale loop always sees
	 * its own token as stopped even if a new session starts in the meantime.
	 */
	interface StopToken {
		stopped: boolean;
		promise: Promise<void>;
		resolve: () => void;
		/** Scoped to this session, not shared, so a stale loop suspended on
		 *  an await from a previous session can never block a fresh
		 *  session's loop from starting. */
		loopRunning: boolean;
	}

	function createStopToken(): StopToken {
		let resolve: () => void = () => {};
		const promise = new Promise<void>((r) => {
			resolve = r;
		});
		return { stopped: false, promise, resolve, loopRunning: false };
	}

	function stop(token: StopToken) {
		if (token.stopped) return;
		token.stopped = true;
		token.resolve();
	}

	let currentStop: StopToken = {
		stopped: true,
		promise: Promise.resolve(),
		resolve: () => {},
		loopRunning: false,
	};

	function emit<K extends keyof PlaybackEngineEvents>(
		event: K,
		...args: Parameters<NonNullable<PlaybackEngineEvents[K]>>
	) {
		for (const l of listeners) {
			const handler = l[event] as ((...a: unknown[]) => void) | undefined;
			handler?.(...args);
		}
	}

	/** Resolves on the next `event` from `target`, or immediately once
	 *  `token` is stopped. A paused element never fires `timeupdate`, so
	 *  every wait in the append loop must race the stop signal or
	 *  `teardown()` leaves the loop hanging forever. */
	function waitFor(target: EventTarget, event: string, token: StopToken): Promise<void> {
		if (token.stopped) return Promise.resolve();
		return new Promise<void>((resolve) => {
			const cleanup = () => target.removeEventListener(event, handler);
			function handler() {
				cleanup();
				resolve();
			}
			target.addEventListener(event, handler);
			token.promise.then(() => {
				cleanup();
				resolve();
			});
		});
	}

	function currentPosition(): { track: PlacedTrack; trackTime: number } | null {
		return trackTimeFor(placed, element.currentTime);
	}

	function onTimeUpdate() {
		const pos = currentPosition();
		if (pos && pos.track.trackId !== lastTrackId) {
			lastTrackId = pos.track.trackId;
			emit("trackchange", lastTrackId);
		}
		emit("timeupdate");
	}

	function onEnded() {
		emit("ended");
	}

	// Registered once — the element is injected and lives for the engine's
	// whole lifetime, independent of individual load()/teardown() cycles.
	element.addEventListener("timeupdate", onTimeUpdate);
	element.addEventListener("ended", onEnded);
	element.addEventListener("seeking", onSeeking);

	function onSourceBufferError(err: unknown) {
		emit("error", err);
	}

	async function attach(manifest: GaplessManifest, token: StopToken): Promise<void> {
		// ManagedMediaSource refuses to attach while remote playback is
		// enabled. Measured on device: this does not break AirPlay, since
		// system-level routing still works.
		element.disableRemotePlayback = true;

		const ms = new mediaSourceImpl();
		mediaSource = ms;
		(element as unknown as { srcObject: unknown }).srcObject = ms;

		await waitFor(ms, "sourceopen", token);

		sourceBufferMime = LOSSLESS_MIME[manifest.codec];
		const sb = ms.addSourceBuffer(sourceBufferMime) as unknown as MinimalSourceBuffer;
		sb.mode = "segments";
		sb.addEventListener("error", onSourceBufferError);
		sourceBuffer = sb;
	}

	function enqueueJob(track: PlacedTrack, manifest: GaplessManifest) {
		manifests.set(track.trackId, manifest);
		jobs.push({
			trackId: track.trackId,
			offsetSeconds: track.offsetSeconds,
			url: manifest.url,
			initByteEnd: manifest.initByteEnd,
			fragments: manifest.fragments,
			initAppended: false,
			fragIndex: 0,
		});
	}

	/** Seconds buffered ahead of the playhead IN THE RANGE THAT CONTAINS IT.
	 *  After a seek the buffer has several ranges, and the last one can be
	 *  minutes ahead of a playhead that is sitting in a hole. Reading that
	 *  range would make the loop wait on a stalled element forever. */
	function bufferedAhead(): number {
		const sb = sourceBuffer;
		if (!sb) return 0;
		const t = element.currentTime;
		for (let i = 0; i < sb.buffered.length; i++) {
			// Small tolerance: a range may start a few ms after the seek target.
			if (sb.buffered.start(i) - 0.25 <= t && t <= sb.buffered.end(i)) {
				return sb.buffered.end(i) - t;
			}
		}
		return 0;
	}

	/** No start tolerance, unlike bufferedAhead: a target just before a range
	 *  is a hole the element stalls in, and a rebuild (which starts one
	 *  fragment early) is the only way out. */
	function isBuffered(t: number): boolean {
		const sb = sourceBuffer;
		if (!sb) return false;
		for (let i = 0; i < sb.buffered.length; i++) {
			if (sb.buffered.start(i) <= t && t < sb.buffered.end(i)) return true;
		}
		return false;
	}

	function jobFor(track: PlacedTrack, fromFragment: number): AppendJob {
		const m = manifests.get(track.trackId);
		if (!m) throw new Error(`createMseEngine: no manifest for ${track.trackId}`);
		return {
			trackId: track.trackId,
			offsetSeconds: track.offsetSeconds,
			url: m.url,
			initByteEnd: m.initByteEnd,
			fragments: m.fragments,
			initAppended: false,
			fragIndex: Math.max(0, Math.min(fromFragment, m.fragments.length - 1)),
		};
	}

	function onSeeking() {
		if (!sourceBuffer || currentStop.stopped) return;
		const t = element.currentTime;
		if (isBuffered(t)) return;
		const pos = trackTimeFor(placed, t);
		if (!pos) return;
		const idx = placed.indexOf(pos.track);
		const fromFragment = Math.floor(pos.trackTime / FRAGMENT_SECONDS) - 1;

		// Stop the running loop, rebuild from the target, restart under a new
		// token so a loop suspended on an await from before the seek cannot
		// append stale fragments behind us.
		stop(currentStop);
		const token = createStopToken();
		currentStop = token;
		jobs = [jobFor(pos.track, fromFragment), ...placed.slice(idx + 1).map((p) => jobFor(p, 0))];
		void runLoop(token);
	}

	function discardNext(): void {
		if (!sourceBuffer || currentStop.stopped) return;
		const pos = currentPosition();
		if (!pos) return;
		const idx = placed.indexOf(pos.track);
		if (idx === placed.length - 1) return;

		const keep = pos.track;
		const end = keep.offsetSeconds + durationSeconds(keep);
		for (const dropped of placed.slice(idx + 1)) manifests.delete(dropped.trackId);
		placed = placed.slice(0, idx + 1);
		trimFrom = end;

		// Resume the kept track after what will survive the trim: the range
		// under the playhead, cut at `end`. The rebuilt job re-sends the init
		// segment, which the media needs since the dropped track's was last.
		const survivesTo = Math.min(pos.trackTime + bufferedAhead(), durationSeconds(keep));
		const fromFragment = Math.floor(survivesTo / FRAGMENT_SECONDS);
		const fragmentCount = manifests.get(keep.trackId)?.fragments.length ?? 0;

		stop(currentStop);
		const token = createStopToken();
		currentStop = token;
		jobs = fromFragment < fragmentCount ? [jobFor(keep, fromFragment)] : [];
		void runLoop(token);
	}

	/** Waits until `sb` is not mid-append/remove. A seek stops the old loop
	 *  while its append may still be in flight on the SAME SourceBuffer, and
	 *  appendBuffer/remove throw InvalidStateError while `updating`. Not
	 *  `abort()`: it throws during a remove() and resets the segment parser. */
	async function whenIdle(sb: MinimalSourceBuffer, token: StopToken): Promise<void> {
		while (sb.updating && !token.stopped) await waitFor(sb, "updateend", token);
	}

	async function appendAndWait(bytes: ArrayBuffer, token: StopToken): Promise<void> {
		const sb = sourceBuffer;
		if (!sb) return;
		await whenIdle(sb, token);
		if (token.stopped) return;
		const done = waitFor(sb, "updateend", token);
		sb.appendBuffer(bytes);
		await done;
	}

	async function evictBehind(token: StopToken): Promise<void> {
		const sb = sourceBuffer;
		if (!sb || sb.buffered.length === 0) return;
		const start = sb.buffered.start(0);
		// Never evict up to or past the playhead — only what has already
		// played, LEAD_SECONDS behind it.
		const cutoff = Math.min(element.currentTime, element.currentTime - LEAD_SECONDS);
		if (cutoff > start) {
			await whenIdle(sb, token);
			// A loop stopped by a seek must not touch the shared buffer.
			if (token.stopped) return;
			const done = waitFor(sb, "updateend", token);
			sb.remove(start, cutoff);
			await done;
		}
	}

	async function runLoop(token: StopToken): Promise<void> {
		if (token.loopRunning) return;
		token.loopRunning = true;
		try {
			while (!token.stopped) {
				if (!sourceBuffer) break;

				if (trimFrom !== null) {
					const sb = sourceBuffer;
					await whenIdle(sb, token);
					if (token.stopped) break;
					const done = waitFor(sb, "updateend", token);
					sb.remove(trimFrom, Number.POSITIVE_INFINITY);
					trimFrom = null;
					await done;
					continue;
				}

				const job = jobs[0];
				if (!job) break;

				if (sourceBuffer.timestampOffset !== job.offsetSeconds) {
					// Setting timestampOffset while `updating` throws, and a
					// cross-track seek can land while the old loop's append is
					// still in flight.
					const sb = sourceBuffer;
					await whenIdle(sb, token);
					if (token.stopped) break;
					sb.timestampOffset = job.offsetSeconds;
				}

				if (!job.initAppended) {
					// Backpressure: append only while the buffer is within
					// LEAD_SECONDS of the playhead.
					while (bufferedAhead() > LEAD_SECONDS && !token.stopped) {
						await waitFor(element, "timeupdate", token);
					}
					if (token.stopped) break;
					const bytes = await fetchRange(job.url, 0, job.initByteEnd);
					if (token.stopped) break;
					await appendAndWait(bytes, token);
					job.initAppended = true;
					continue;
				}

				if (job.fragIndex >= job.fragments.length) {
					jobs.shift();
					continue;
				}

				while (bufferedAhead() > LEAD_SECONDS && !token.stopped) {
					await waitFor(element, "timeupdate", token);
				}
				if (token.stopped) break;

				const frag = job.fragments[job.fragIndex];
				const bytes = await fetchRange(job.url, frag.start, frag.end);
				if (token.stopped) break;
				await appendAndWait(bytes, token);
				if (token.stopped) break;
				job.fragIndex++;
				await evictBehind(token);
			}
		} catch (err) {
			emit("error", err);
		} finally {
			token.loopRunning = false;
		}
	}

	async function load(track: PlayableTrack): Promise<void> {
		const { trackId, versionId, manifest } = track;
		if (!manifest) {
			// selectEngine decides which engine gets a track upstream, based on
			// exactly this same manifest presence — a caller reaching this
			// engine without one is a programming error, not a runtime state.
			throw new Error("createMseEngine: load() requires track.manifest");
		}

		stop(currentStop);
		placed = [];
		jobs = [];
		manifests.clear();
		trimFrom = null;
		lastTrackId = trackId;
		mediaSource = null;
		sourceBuffer = null;
		sourceBufferMime = null;

		const token = createStopToken();
		currentStop = token;

		await attach(manifest, token);
		const placedTrack = placeTrack(placed, trackId, versionId, manifest);
		placed.push(placedTrack);
		enqueueJob(placedTrack, manifest);
		void runLoop(token);
	}

	async function prepareNext(track: PlayableTrack): Promise<void> {
		const { manifest } = track;
		if (!manifest) {
			// See load(): selectEngine already decided this engine only sees
			// tracks with a manifest.
			throw new Error("createMseEngine: prepareNext() requires track.manifest");
		}

		if (!mediaSource) throw new Error("createMseEngine: call load() before prepareNext()");
		const placedTrack = placeTrack(placed, track.trackId, track.versionId, manifest);
		placed.push(placedTrack);
		enqueueJob(placedTrack, manifest);
		void runLoop(currentStop);
	}

	function canAppend(track: PlayableTrack): boolean {
		const manifest = track.manifest;
		if (!manifest) return false;
		if (!sourceBufferMime) return true;
		return LOSSLESS_MIME[manifest.codec] === sourceBufferMime;
	}

	async function play(): Promise<void> {
		await element.play();
	}

	function pause(): void {
		element.pause();
	}

	function seekToTrackTime(seconds: number): void {
		const track = currentPosition()?.track ?? placed[0];
		if (!track) return;
		element.currentTime = elementTimeFor(track, seconds);
	}

	function getTrackTime(): number {
		return currentPosition()?.trackTime ?? 0;
	}

	function getTrackDuration(): number {
		const pos = currentPosition();
		return pos ? durationSeconds(pos.track) : 0;
	}

	function setVolume(v: number): void {
		element.volume = v;
	}

	function teardown(): void {
		stop(currentStop);
		if (sourceBuffer) {
			sourceBuffer.removeEventListener("error", onSourceBufferError);
		}
		element.removeEventListener("timeupdate", onTimeUpdate);
		element.removeEventListener("ended", onEnded);
		element.removeEventListener("seeking", onSeeking);
		try {
			(element as unknown as { srcObject: unknown }).srcObject = null;
		} catch {
			// jsdom / fakes may not support clearing srcObject; not fatal.
		}
		mediaSource = null;
		sourceBuffer = null;
		sourceBufferMime = null;
		jobs = [];
		placed = [];
	}

	function subscribe(events: PlaybackEngineEvents): () => void {
		listeners.add(events);
		return () => listeners.delete(events);
	}

	return {
		load,
		play,
		pause,
		seekToTrackTime,
		getTrackTime,
		getTrackDuration,
		setVolume,
		canAppend,
		prepareNext,
		discardNext,
		teardown,
		subscribe,
	};
}
