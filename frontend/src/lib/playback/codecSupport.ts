export type LosslessCodec = "alac" | "flac";

/** ALAC plays on Safari, FLAC on Chrome and Firefox. Neither engine supports
 *  the other, which is why the backend stores both. */
export const LOSSLESS_MIME: Record<LosslessCodec, string> = {
	alac: 'audio/mp4; codecs="alac"',
	flac: 'audio/mp4; codecs="flac"',
};

/** Preference order. ALAC is first so Safari, which supports only ALAC, is
 *  never asked to consider anything else. */
const ORDER: LosslessCodec[] = ["alac", "flac"];

function defaultImpl(): typeof MediaSource | undefined {
	if (typeof window === "undefined") return undefined;
	const w = window as unknown as {
		ManagedMediaSource?: typeof MediaSource;
		MediaSource?: typeof MediaSource;
	};
	return w.ManagedMediaSource ?? w.MediaSource;
}

/**
 * Which lossless codecs this browser can actually play through MSE.
 *
 * Decided by isTypeSupported and never by user agent. User-agent sniffing is
 * how this breaks on the next browser release.
 */
export function supportedLosslessCodecs(
	impl: typeof MediaSource | undefined = defaultImpl(),
): LosslessCodec[] {
	if (!impl || typeof impl.isTypeSupported !== "function") return [];
	return ORDER.filter((codec) => impl.isTypeSupported(LOSSLESS_MIME[codec]));
}

/**
 * The value for the API's `codecs` parameter, or null when there is nothing
 * to ask for.
 *
 * Null, not an empty string: the server treats a present-but-empty `codecs`
 * as an opt-in to the gapless branch, and an unsupported browser must not
 * opt in at all.
 */
export function codecsParam(
	impl: typeof MediaSource | undefined = defaultImpl(),
): string | null {
	const codecs = supportedLosslessCodecs(impl);
	return codecs.length > 0 ? codecs.join(",") : null;
}
