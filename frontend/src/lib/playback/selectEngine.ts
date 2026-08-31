import type { LosslessCodec } from "@/lib/playback/codecSupport";
import type { GaplessManifest } from "@/lib/playback/types";

export type EngineKind = "mse" | "elementPair";

/**
 * Which engine plays this track.
 *
 * The server decides availability — it sends a manifest only when the
 * resolved quality is lossless and a completed segment set exists. The client
 * decides capability. Both must agree.
 */
export function selectEngine(input: {
	manifest?: GaplessManifest | null;
	supported: LosslessCodec[];
}): EngineKind {
	const { manifest, supported } = input;
	if (!manifest) return "elementPair";
	if (!supported.includes(manifest.codec)) return "elementPair";
	return "mse";
}

/**
 * Whether the next track can join the current timeline without a teardown.
 *
 * Only an MSE-to-MSE transition is gapless. Everything else tears one engine
 * down and starts another, which costs the same seam the lossy tier has
 * always had. That is the accepted cost of a mixed queue.
 */
export function canAppendNext(current: EngineKind, next: EngineKind): boolean {
	return current === "mse" && next === "mse";
}
