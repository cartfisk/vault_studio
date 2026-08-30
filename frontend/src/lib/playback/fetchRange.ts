import { resolveApiUrl } from "@/api/server";
import { getAuthorizationHeader } from "@/api/session";

/**
 * Fetches one byte range of a fragmented MP4 for the MSE engine.
 *
 * Auth mirrors `apiClient` in `@/api/client`: cookie via `credentials:
 * "include"` for the web build, plus the bearer token from
 * `getAuthorizationHeader()` for the Capacitor build. This intentionally
 * does NOT reuse `resolveApiMediaUrl` from `@/api/media` — that helper also
 * appends `access_token` as a query param, which exists for elements (like
 * `<audio src>`) that can't set headers. Range fetches go through `fetch()`
 * directly and can set the Authorization header, so a query-param token
 * would just be a redundant signed-URL-style auth path. Only the base-URL
 * resolution from `resolveApiUrl` is reused here.
 *
 * `start` and `end` are both inclusive byte offsets, taken verbatim from the
 * server's manifest — never adjusted here.
 */
export async function fetchRange(
	url: string,
	start: number,
	end: number,
): Promise<ArrayBuffer> {
	const res = await fetch(resolveApiUrl(url), {
		credentials: "include",
		headers: {
			Range: `bytes=${start}-${end}`,
			...getAuthorizationHeader(),
		},
	});

	if (res.status !== 206) {
		throw new Error(
			`fetchRange: expected 206 for bytes=${start}-${end}, got ${res.status}`,
		);
	}

	return await res.arrayBuffer();
}
