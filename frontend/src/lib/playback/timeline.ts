import type { GaplessManifest, PlacedTrack } from "@/lib/playback/types";

/**
 * Position a track after everything already placed.
 *
 * The offset is a running sum of true sample counts. It is NOT
 * `duration * index` — track durations vary, and that mistake puts every
 * later track in the queue at the wrong place.
 */
export function placeTrack(
	placed: PlacedTrack[],
	trackId: string,
	versionId: number | null,
	manifest: GaplessManifest,
): PlacedTrack {
	const last = placed[placed.length - 1];
	const offsetSamples = last ? last.offsetSamples + last.durationSamples : 0;

	return {
		trackId,
		versionId,
		offsetSamples,
		durationSamples: manifest.sampleCount,
		sampleRate: manifest.sampleRate,
	};
}

export function offsetSeconds(track: PlacedTrack): number {
	return track.offsetSamples / track.sampleRate;
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
		const start = offsetSeconds(track);
		if (elementTime >= start) {
			const trackTime = elementTime - start;
			if (trackTime > durationSeconds(track)) return null;
			return { track, trackTime };
		}
	}
	return null;
}

/** Inverse of trackTimeFor: where on the element timeline a track-relative
 *  position lives. Used for seeking. */
export function elementTimeFor(track: PlacedTrack, trackTime: number): number {
	return offsetSeconds(track) + trackTime;
}
