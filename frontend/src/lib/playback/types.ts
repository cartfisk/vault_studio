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

/** One track's position on the shared MSE timeline.
 *
 *  Offsets and durations are in SAMPLES, not seconds, so a long queue cannot
 *  accumulate float error. Seconds are derived only at the boundary, by
 *  offsetSeconds/durationSeconds. */
export interface PlacedTrack {
	trackId: string;
	versionId: number | null;
	offsetSamples: number;
	durationSamples: number;
	sampleRate: number;
}
