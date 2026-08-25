/**
 * Pure decision logic for near-gapless playback.
 *
 * Deliberately DOM-free and network-free so it can be unit-tested in Vitest's
 * default `node` environment. All browser mechanics live in MusicPlayer.
 */

/** How long before a track ends we start buffering the next one. */
export const PRELOAD_LEAD_SECONDS = 20;

/**
 * Mirrors the server's SIGNED_URL_TTL (docker-compose.yml). Duplicated across
 * the client/server boundary; the safety margin below absorbs drift.
 */
export const SIGNED_URL_TTL_SECONDS = 300;

/** Re-sign this many seconds before the URL actually expires. */
const DEFAULT_SAFETY_MARGIN_SECONDS = 30;

/** HTMLMediaElement.HAVE_FUTURE_DATA — enough buffered to start playing. */
export const SWAP_MIN_READY_STATE = 3;

/**
 * 10ms of silence, 8kHz mono PCM. Used only to give an otherwise-empty
 * <audio> element a playable source so iOS will accept the gesture unlock.
 */
export const SILENT_AUDIO_DATA_URI =
	"data:audio/wav;base64,UklGRsQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YaAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";

export interface PreloadTriggerInput {
	currentTime: number;
	duration: number;
	leadSeconds?: number;
}

/**
 * True once the current track is within `leadSeconds` of its end.
 *
 * Returns false whenever duration or currentTime are unusable — before
 * `loadedmetadata` fires, duration is 0 or NaN and we must not guess.
 */
export function shouldStartPreload({
	currentTime,
	duration,
	leadSeconds = PRELOAD_LEAD_SECONDS,
}: PreloadTriggerInput): boolean {
	if (!Number.isFinite(duration) || duration <= 0) return false;
	if (!Number.isFinite(currentTime) || currentTime < 0) return false;
	return duration - currentTime <= leadSeconds;
}

export interface PreloadStaleInput {
	signedAt: number;
	now: number;
	ttlSeconds?: number;
	safetyMarginSeconds?: number;
}

/**
 * True when a signed stream URL is close enough to expiry that we should
 * re-mint rather than risk a 403 at swap time. Happens when the user pauses
 * inside the preload window and resumes minutes later.
 */
export function isPreloadStale({
	signedAt,
	now,
	ttlSeconds = SIGNED_URL_TTL_SECONDS,
	safetyMarginSeconds = DEFAULT_SAFETY_MARGIN_SECONDS,
}: PreloadStaleInput): boolean {
	const ageSeconds = (now - signedAt) / 1000;
	return ageSeconds >= ttlSeconds - safetyMarginSeconds;
}

export type Transition = "swap" | "load";

export interface TransitionInput {
	standbySrc: string | null;
	targetUrl: string;
	readyState: number;
}

/**
 * Decides whether we can hand over to the already-buffered standby element or
 * must fall back to loading into the active element.
 */
export function chooseTransition({
	standbySrc,
	targetUrl,
	readyState,
}: TransitionInput): Transition {
	if (!standbySrc) return "load";
	if (standbySrc !== targetUrl) return "load";
	if (readyState < SWAP_MIN_READY_STATE) return "load";
	return "swap";
}
