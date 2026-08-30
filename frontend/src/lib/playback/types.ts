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
