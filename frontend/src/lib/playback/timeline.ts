import type { GaplessManifest, PlacedTrack } from "@/lib/playback/types";

/**
 * Position a track after everything already placed.
 *
 * The offset is a running sum of each track's own duration in seconds. It is
 * NOT a sum of raw sample counts — sample counts from tracks at different
 * sample rates are not the same unit and cannot be summed directly. It is
 * also NOT `duration * index` — track durations vary, and that mistake puts
 * every later track in the queue at the wrong place.
 */
export function placeTrack(
	placed: PlacedTrack[],
	trackId: string,
	versionId: number | null,
	manifest: GaplessManifest,
): PlacedTrack {
	const last = placed[placed.length - 1];
	const offsetSeconds = last ? last.offsetSeconds + durationSeconds(last) : 0;

	return {
		trackId,
		versionId,
		offsetSeconds,
		durationSamples: manifest.sampleCount,
		sampleRate: manifest.sampleRate,
	};
}

export function durationSeconds(track: PlacedTrack): number {
	return track.durationSamples / track.sampleRate;
}

/**
 * Map a position on the shared element timeline back to a track and a
 * track-relative time.
 *
 * This is the function that keeps the offset from escaping. Everything the UI
 * displays comes through here.
 *
 * A position exactly on a boundary belongs to the LATER track: at the instant
 * track A's last sample has played, the playhead is at track B's first.
 */
export function trackTimeFor(
	placed: PlacedTrack[],
	elementTime: number,
): { track: PlacedTrack; trackTime: number } | null {
	for (let i = placed.length - 1; i >= 0; i--) {
		const track = placed[i];
		const start = track.offsetSeconds;
		if (elementTime >= start) {
			const trackTime = elementTime - start;
			// elementTime comes from the browser's reported currentTime;
			// durationSeconds is computed independently from the manifest. They
			// need not agree to the last bit, so allow one sample period of
			// slack at the track's own rate — otherwise a legitimate final
			// sample can fall fractionally outside and wrongly return null.
			const epsilon = 1 / track.sampleRate;
			if (trackTime > durationSeconds(track) + epsilon) return null;
			return { track, trackTime };
		}
	}
	return null;
}

/** Inverse of trackTimeFor: where on the element timeline a track-relative
 *  position lives. Used for seeking. */
export function elementTimeFor(track: PlacedTrack, trackTime: number): number {
	return track.offsetSeconds + trackTime;
}
