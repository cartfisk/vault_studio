/** A byte range from the backend manifest. Both ends are INCLUSIVE, matching
 *  HTTP `Range: bytes=start-end`. The client never does byte arithmetic on
 *  these — they are used verbatim. */
export interface FragmentRange {
	start: number;
	end: number;
}

/** The `gapless` object returned by GET /api/media/stream/{id}?codecs=... */
export interface GaplessManifest {
	codec: "alac" | "flac";
	/** Already carries its own version_id. Use verbatim; do not rebuild it. */
	url: string;
	sampleRate: number;
	sampleCount: number;
	channels: number;
	/** Inclusive last byte of the ftyp+moov prelude. */
	initByteEnd: number;
	fragments: FragmentRange[];
}

/** One track's position on the shared MSE timeline. */
export interface PlacedTrack {
	trackId: string;
	versionId: number | null;
	/** Seconds from the start of the shared timeline.
	 *
	 *  Seconds, not samples: sample counts from tracks at different rates are
	 *  not the same unit and cannot be summed. Each track's duration is
	 *  computed exactly at its own rate, and only those seconds are added.
	 *  Float accumulation is safe here — summing 1000 track durations drifts
	 *  about 1.5e-11s, six orders of magnitude below one sample period. */
	offsetSeconds: number;
	durationSamples: number;
	sampleRate: number;
}

export interface PlaybackEngineEvents {
	timeupdate?: () => void;
	trackchange?: (trackId: string) => void;
	ended?: () => void;
	error?: (err: unknown) => void;
}

/** A track as handed to an engine.
 *
 *  `url` is always present — it is what the element-pair engine plays and
 *  what any engine falls back to. `manifest` is present only when the server
 *  offered a lossless rendition this browser can play, which is the sole
 *  input to whether the MSE engine can take the track. */
export interface PlayableTrack {
	trackId: string;
	versionId: number | null;
	url: string;
	manifest?: GaplessManifest | null;
}

/**
 * The shared contract implemented by every playback engine (MSE, and later
 * an element-pair engine for mixed/lossy queues).
 *
 * Kept free of anything MSE-specific — no MediaSource, no SourceBuffer, no
 * byte ranges in the signatures — so a second implementation can satisfy it
 * without leaking implementation details into callers.
 */
export interface PlaybackEngine {
	load(track: PlayableTrack): Promise<void>;
	play(): Promise<void>;
	pause(): void;
	seekToTrackTime(seconds: number): void;
	getTrackTime(): number;
	getTrackDuration(): number;
	setVolume(v: number): void;
	canAppend(track: PlayableTrack): boolean;
	prepareNext(track: PlayableTrack): Promise<void>;
	teardown(): void;
	subscribe(events: PlaybackEngineEvents): () => void;
}
