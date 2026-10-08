# Gapless Lossless — Finish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lossless tracks play through the MSE engine with no audible seam at a track boundary, and scrubbing, queue edits, pause/resume, and the iOS cold-start unlock all keep working while it does.

**Architecture:** The engine, selection, and timeline code on PR #6 stay as they are. This plan removes the client-side quality decision that kept the engine from ever being selected (the server already resolves quality from stored preferences and project override when the request omits it), makes the context read engine kind from the one place MusicPlayer publishes it, and closes three real holes in `mseEngine`: no seek handling, no way to retract an appended next track, and a buffered-ahead calculation that is wrong once the buffer has more than one range. Everything is verified in Chrome against the deployed backend before any device run.

**Tech Stack:** React 19, TypeScript, Vitest + jsdom + Testing Library, Media Source Extensions, Go backend (unchanged).

## Global Constraints

- MP3 (element-pair) playback must be byte-for-byte unchanged in behaviour. `MusicPlayer.swapOrder.test.tsx` must pass unmodified.
- Every new test is verified by breaking the code it covers and watching it fail. Three tests on this feature previously passed either way.
- `cmd | tail` returns `tail`'s exit code. Run checks bare, or `set -o pipefail`.
- Do not run `npm install` in `frontend/`. Use `npm install --no-package-lock` if `node_modules` is missing; the committed lockfile is out of sync and a plain install rewrites it.
- Run frontend checks from `frontend/` with `./node_modules/.bin/tsc --noEmit -p .` and `./node_modules/.bin/vitest run`. Discard `src/routeTree.gen.ts` churn after a vitest run (`git checkout -- src/routeTree.gen.ts`).
- No client-side trimming, no gapless UI indicator, no `SharedTrackPlayer` support (design non-goals).
- Commits need explicit permission in the session. Each task ends with a proposed commit message; do not commit without being told to.

## What this plan supersedes

- `stash@{0}` ("diagnostics + preferences gate + render-loop guard"). Do not apply it. The preferences gate is replaced by Task 1 (the stash gated on `isLoading`, which `PreferencesContext` flips to `false` on the unauthenticated first pass and never back to `true`, so the gate would have passed with `preferences` still `null`). The functional-updater "render loop guard" is a no-op: React already bails out of a `useState` set whose value is `Object.is`-equal to the current one. The stable ref callback is reapplied by hand in Task 2. After Task 2 lands the stash can be dropped.
- The "quality race" fix in the handoff. The race is real, the fix here is different.

## Findings the plan rests on

1. **The client sends a `quality` it guessed.** `AudioPlayerContext` sends `quality: preferences?.default_quality || "lossy"`. On a cold start `preferences` is `null` until the fetch resolves, so the first track asks for `lossy`, the server correctly withholds the manifest, and the track is pinned to the element pair. `resolveQuality` in `internal/handlers/streaming.go:133` already resolves an omitted `quality` as: project `QualityOverride`, then the user's stored `default_quality`, then `lossy`. The client-side copy is redundant, races, and overrides the project-level setting (a pre-existing bug). Delete it.
2. **Engine kind is derived twice** (`currentEngineKindRef` in the context, `desiredEngine` in MusicPlayer). MusicPlayer already publishes `kind` on the facade in `audioPlayerRef`. The context should read that.
3. **`mseEngine` cannot seek.** `evictBehind` removes everything more than 30s behind the playhead. A seek back into evicted media, or forward past the buffered end, stalls forever: the append loop only walks fragments sequentially from where it is. There is no `seeking` listener. This is device check 6 ("scrubbing") and it is broken by construction, not by a regression.
4. **`bufferedAhead` reads the last range only.** After a forward seek the buffer has two ranges; `buffered.end(length-1)` is the old data far ahead of the playhead, `bufferedAhead` reads as > 30s, and the loop waits on `timeupdate` while the element is stalled at an unbuffered position. Deadlock.
5. **An appended next track cannot be retracted.** Queue reorder/remove (upstream just added drag-to-reorder in the fullscreen player) changes `getNextTrack()`, the preload key changes, and `prepareNext` is called again for the new next — appended *after* the stale one. The stale track plays at the boundary.
6. **`unlockMseElement` never succeeds and never stops retrying.** It attaches a throwaway `MediaSource` and waits on `play()`, which cannot resolve on a source with no data. The engine's later `load()` replaces `srcObject` and the pending `play()` rejects with `AbortError`. `mseUnlockedRef` stays false, the `pointerdown` listener stays attached, and every later gesture re-fires the unlock, which **does** overwrite a live `srcObject` once the engine exists. iOS registers the gesture unlock at the `play()` *call*, not at resolution.
7. **`isPlaying` effect gates on `audio.src`.** Under MSE the element has `srcObject` and an empty `src`, so `setIsPlaying(true)` without an explicit `engine.play()` does nothing. Today every path also calls `play()` directly so this is latent; it is a one-line fix and Task 2 takes it.

## File Structure

- Modify `frontend/src/contexts/AudioPlayerContext.tsx` — drop the quality decision (Task 1), read engine kind from the facade (Task 3), retract a stale append (Task 5).
- Modify `frontend/src/contexts/AudioPlayerContext.test.tsx` — tests for Tasks 1, 3, 5.
- Modify `frontend/src/components/MusicPlayer.tsx` — stable ref callback, loud latch, `srcObject` guard (Task 2), unlock guard (Task 6), publish `discardNext` (Task 5), debug logging (Task 7).
- Modify `frontend/src/components/MusicPlayer.engineSelection.test.tsx` — tests for Tasks 2, 6.
- Modify `frontend/src/lib/playback/types.ts` — add `discardNext()` to `PlaybackEngine` (Task 5).
- Modify `frontend/src/lib/playback/mseEngine.ts` — `bufferedAhead` over the containing range (Task 4), `seeking` handler (Task 4), `discardNext` (Task 5).
- Modify `frontend/src/lib/playback/mseEngine.test.ts` — tests for Tasks 4, 5.
- Modify `frontend/src/lib/playback/elementPairEngine.ts` — no-op `discardNext` (Task 5).
- Modify `frontend/vite.config.ts` — env-driven proxy target (Task 0).
- Modify `docs/superpowers/plans/2026-08-25-gapless-playback-device-checklist.md` — results (Task 8).

## Execution order

Task 0 → 1 → 2 → 3 → 4 → 5 → 6 → 7 (Chrome verification, the first real evidence) → 8 (device). Tasks 1–6 are each independently landable; 7 is where the gap is either gone or it is not, and nothing in 1–6 should be considered proven before 7.

---

### Task 0: Dev proxy to the deployed backend

**Files:**
- Modify: `frontend/vite.config.ts:32-34`

The deployed server sets `CORS_ALLOWED_ORIGINS` to the public host and secure/`Lax` cookies, so the browser must stay on one origin and proxy. This is the uncommitted change from the previous session's worktree, committed.

- [ ] **Step 1: Apply the change**

```ts
      '/api': {
        // Point the dev server at a real backend with:
        //   VITE_PROXY_TARGET=http://<unraid-ip>:8081 npm run dev
        // Proxying (rather than calling the backend directly) keeps the browser
        // on one origin, which sidesteps both CORS and the Secure/SameSite
        // cookie rules the deployed server enforces.
        target: process.env.VITE_PROXY_TARGET || 'http://localhost:8080',
```

- [ ] **Step 2: Verify**

Run from `frontend/`: `./node_modules/.bin/tsc --noEmit -p .` — exit 0.

- [ ] **Step 3: Propose commit**

```
Let the dev proxy target be set from the environment
```

---

### Task 1: Stop sending `quality`; let the server resolve it

**Files:**
- Modify: `frontend/src/contexts/AudioPlayerContext.tsx:159` (`DEFAULT_AUDIO_QUALITY`), `:225-226` (`usePreferences`), `:397` (`const quality`), `:427-434` (`getStreamUrl` in `play`), `:1044` (preload key), `:1069-1075` (`getStreamUrl` in preload), `:1133` (deps)
- Test: `frontend/src/contexts/AudioPlayerContext.test.tsx`

**Interfaces:**
- Consumes: `getStreamUrl(trackId, { versionId?, codecs? })` from `../api/media` — `quality` is optional and already omitted from the query when absent.
- Produces: nothing new. `preferences.default_quality` stays in the preload key only so a quality toggle re-arms the preload.

- [ ] **Step 1: Write the failing tests**

Add to `AudioPlayerContext.test.tsx`, next to the `codecs parameter` describe. The existing `usePreferences` mock returns `{ preferences: { default_quality: "lossless" } }`; change it to `vi.hoisted` state so a test can set `preferences: null`:

```tsx
const prefs = vi.hoisted(() => ({
	value: { default_quality: "lossless" } as { default_quality: string } | null,
}));

vi.mock("./PreferencesContext", () => ({
	usePreferences: () => ({ preferences: prefs.value, isLoading: false }),
}));
```

(Replace the existing `vi.mock("./PreferencesContext", ...)` block. Reset `prefs.value` in `beforeEach`.)

```tsx
describe("quality parameter", () => {
	it("never sends quality: the server resolves it from stored preferences", async () => {
		prefs.value = { default_quality: "source" };
		mount();

		await act(async () => {
			await ctx.play(TRACKS[0], TRACKS, false);
		});

		const params = apis.getStreamUrl.mock.calls[0][1];
		expect("quality" in params).toBe(false);
	});

	it("still mints a URL before preferences have loaded", async () => {
		// Cold start: preferences are null until the fetch resolves. Playing
		// must not wait on them and must not guess a tier.
		prefs.value = null;
		mount();

		await act(async () => {
			await ctx.play(TRACKS[0], TRACKS, false);
		});

		expect(apis.getStreamUrl).toHaveBeenCalledTimes(1);
		const params = apis.getStreamUrl.mock.calls[0][1];
		expect("quality" in params).toBe(false);
	});
});
```

- [ ] **Step 2: Run to verify they fail**

Run: `./node_modules/.bin/vitest run src/contexts/AudioPlayerContext.test.tsx -t "quality parameter"`
Expected: both FAIL — `"quality" in params` is `true`.

- [ ] **Step 3: Implement**

In `AudioPlayerContext.tsx`:

- Delete `const DEFAULT_AUDIO_QUALITY = "lossy";`.
- Replace `const qualityPreference = preferences?.default_quality || DEFAULT_AUDIO_QUALITY;` with:

```ts
  /**
   * Only used to re-arm the preload when the user toggles quality. The
   * stream request itself never carries a quality: `resolveQuality` on the
   * server applies project override, then the stored preference, then
   * lossy. Sending a client-side guess raced the preference fetch (a cold
   * start asked for lossy and lost the gapless manifest for the first
   * track) and silently overrode the project-level setting.
   */
  const qualityPreference = preferences?.default_quality ?? null;
```

- In `play`, delete `const quality = qualityPreference;` and remove `quality,` from the `getStreamUrl` call.
- In the preload effect, remove `quality: qualityPreference,` from the `getStreamUrl` call. Keep `qualityPreference` in the key string and the dependency array.

- [ ] **Step 4: Run the whole context suite and typecheck**

Run: `./node_modules/.bin/vitest run src/contexts/AudioPlayerContext.test.tsx` then `./node_modules/.bin/tsc --noEmit -p .`
Expected: all pass, tsc exit 0.

- [ ] **Step 5: Mutation check**

Temporarily put `quality: "lossy",` back into the `play` call. The first new test must fail. Revert.

- [ ] **Step 6: Propose commit**

```
Let the server resolve stream quality

- the client sent `preferences.default_quality || "lossy"`, which raced
  the preference fetch and pinned a cold-start track to the element pair
- `resolveQuality` already applies project override, then stored
  preference, when the request omits `quality`
```

---

### Task 2: MusicPlayer hygiene from the last debugging round

**Files:**
- Modify: `frontend/src/components/MusicPlayer.tsx:1470-1478` (inline ref), `:922-928` (silent latch), `:1040-1055` (`isPlaying` effect)
- Test: `frontend/src/components/MusicPlayer.engineSelection.test.tsx`

**Interfaces:**
- Produces: `setMseElement: (el: HTMLAudioElement | null) => void` (stable, `useCallback([])`).

- [ ] **Step 1: Write the failing test**

```tsx
	it("logs loudly when it latches MSE off because no engine could be built", async () => {
		codecSupport.supported = ["alac"];
		// No MediaSource implementation at all: getMseEngine() returns null.
		delete (window as unknown as { MediaSource?: unknown }).MediaSource;
		const error = vi.spyOn(console, "error").mockImplementation(() => {});
		store.set({
			currentPlayable: { trackId: "t1", versionId: null, url: TRACK_A_URL, manifest: MANIFEST },
		});

		render(<MusicPlayer hideControls />);
		await act(async () => {});

		expect(mse.create).not.toHaveBeenCalled();
		expect(error).toHaveBeenCalledWith(
			expect.stringContaining("MSE disabled for this session"),
			expect.anything(),
		);
		error.mockRestore();
	});
```

- [ ] **Step 2: Run to verify it fails**

Run: `./node_modules/.bin/vitest run src/components/MusicPlayer.engineSelection.test.tsx -t "logs loudly"`
Expected: FAIL — `console.error` not called.

- [ ] **Step 3: Implement**

Stable ref callback, declared next to `mseElRef`:

```ts
  /**
   * Stable on purpose. An inline `ref={(el) => ...}` gets a new identity each
   * render, so React detaches (calls with null) and reattaches it on every
   * re-render, and `mseElRef.current` is transiently null in that window.
   */
  const setMseElement = useCallback((el: HTMLAudioElement | null) => {
    mseElRef.current = el;
    if (el) el.disableRemotePlayback = true;
  }, []);
```

JSX: `<audio ref={setMseElement} ...>` replacing the inline arrow.

Loud latch, in the load effect where `getMseEngine()` returns null:

```ts
    if (!mse) {
      // Say so. This disables gapless for the rest of the session; doing it
      // silently once cost a full debugging session.
      console.error("[MusicPlayer] MSE disabled for this session: no engine", {
        elementMounted: !!mseElRef.current,
        trackId: currentPlayable.trackId,
      });
      setMseDisabled(true);
      return;
    }
```

`isPlaying` effect: change `if (audio.paused && audio.src)` to

```ts
      const hasSource =
        !!audio.src || !!(audio as unknown as { srcObject: unknown }).srcObject;
      if (audio.paused && hasSource) {
```

- [ ] **Step 4: Run suites**

Run: `./node_modules/.bin/vitest run src/components/` and `./node_modules/.bin/tsc --noEmit -p .`
Expected: pass, including `swapOrder` unmodified.

- [ ] **Step 5: Propose commit**

```
Stabilise the MSE element ref and make the MSE latch loud

- inline ref callback detached/reattached the element every render
- `setMseDisabled(true)` on a missing engine logged nothing
- `isPlaying` effect treated an MSE element (srcObject, no src) as
  having no source
```

---

### Task 3: One owner for engine kind

**Files:**
- Modify: `frontend/src/contexts/AudioPlayerContext.tsx:89-100` (`ContextEngine`), `:221-223` (`currentEngineKindRef`), `:444-447` (assignment in `play`), `:1099-1101` (`canAppendNext` call)
- Test: `frontend/src/contexts/AudioPlayerContext.test.tsx`

**Interfaces:**
- Consumes: `audioPlayerRef.current.kind: EngineKind`, already published by MusicPlayer (`MusicPlayer.tsx:1083`).
- Produces: `ContextEngine` gains `kind: EngineKind`.

- [ ] **Step 1: Write the failing test**

In `fakeEngine` add `kind: "mse" as "mse" | "elementPair"` and a parameter to override it. Then:

```tsx
	it("hands off when MusicPlayer reports the element pair, even with a manifest", async () => {
		// MusicPlayer latches mseDisabled after a runtime failure. The context
		// must follow what is actually playing, not its own memory of the
		// manifest it saw at play() time.
		codecSupport.codecs = "alac";
		codecSupport.supported = ["alac"];
		apis.getStreamUrl.mockResolvedValue({ url: "/api/stream/x", gapless: MANIFEST });
		mount();
		const engine = { ...fakeEngine(5, true), kind: "elementPair" as const };

		await act(async () => {
			await ctx.play(TRACKS[0], TRACKS, true);
		});
		act(() => {
			ctx.audioPlayerRef.current = engine;
		});
		await act(async () => {
			ctx.onDurationChange(100);
			ctx.onProgressUpdate(95);
		});
		await act(async () => {
			await Promise.resolve();
			await Promise.resolve();
		});

		expect(engine.prepareNext).not.toHaveBeenCalled();
		expect(ctx.nextTrackPreload?.trackId).toBe("t2");
	});
```

- [ ] **Step 2: Run to verify it fails**

Expected: FAIL — `prepareNext` was called (context still trusts `currentEngineKindRef`).

- [ ] **Step 3: Implement**

- Add `| "kind"` to the `ContextEngine` `Pick<...>` union, and make the facade type it is picked from include `kind: EngineKind` if it does not already.
- Delete `currentEngineKindRef` and its assignment in `play` (keep the `selectEngine` import only if still used elsewhere; `play` no longer needs it).
- In the preload effect:

```ts
        const engine = getEngine();
        const append =
          canAppendNext(engine?.kind ?? "elementPair", nextEngineKind) &&
          (engine?.canAppend(playable) ?? false);
```

- [ ] **Step 4: Run suite, typecheck, mutation check**

Mutation: change `engine?.kind ?? "elementPair"` to `"mse"`. The new test must fail. Revert.

- [ ] **Step 5: Propose commit**

```
Read engine kind from the published facade, not a context ref

- `currentEngineKindRef` was set once in `play()` and could disagree
  with MusicPlayer's `desiredEngine` after an MSE latch
```

---

### Task 4: Seeking inside the MSE engine

**Files:**
- Modify: `frontend/src/lib/playback/mseEngine.ts` (`bufferedAhead`, new `onSeeking`, `AppendJob` rebuild, listener registration/teardown)
- Test: `frontend/src/lib/playback/mseEngine.test.ts`

**Interfaces:**
- Consumes: `FragmentDurationMicros = 10_000_000` from the backend (`internal/transcoding/segments.go:16`). Fragments are ≥10s and begin at a packet boundary, so fragment `k` starts at `k*10s` plus up to one codec frame (~93ms at 44.1k). The engine starts one fragment early to cover that.
- Produces: `export const FRAGMENT_SECONDS = 10;`

- [ ] **Step 1: Extend the fakes**

`FakeSourceBuffer` needs multiple ranges. Replace its single start/end with a list:

```ts
class FakeSourceBuffer extends EventTarget {
	mode = "";
	timestampOffset = 0;
	appendCalls: ArrayBuffer[] = [];
	removeCalls: Array<{ start: number; end: number }> = [];
	ranges: Array<{ start: number; end: number }> = [];

	constructor(private secondsPerAppend: number) {
		super();
	}

	get buffered() {
		const ranges = this.ranges;
		return {
			length: ranges.length,
			start: (i: number) => ranges[i].start,
			end: (i: number) => ranges[i].end,
		};
	}

	/** Appends grow the range that starts at `timestampOffset`, or open a
	 *  new one, like a real buffer after a seek. */
	appendBuffer(data: ArrayBuffer): void {
		this.appendCalls.push(data);
		const at = this.timestampOffset;
		const range = this.ranges.find((r) => r.start <= at && at <= r.end);
		if (range) range.end += this.secondsPerAppend;
		else this.ranges.push({ start: at, end: at + this.secondsPerAppend });
		this.ranges.sort((a, b) => a.start - b.start);
		queueMicrotask(() => this.dispatchEvent(new Event("updateend")));
	}

	remove(start: number, end: number): void {
		this.removeCalls.push({ start, end });
		this.ranges = this.ranges
			.map((r) => (r.start >= start && r.end <= end ? null : r.start < start && r.end > start ? { ...r, end: Math.min(r.end, start) } : r.start < end && r.end > end ? { ...r, start: Math.max(r.start, end) } : r))
			.filter((r): r is { start: number; end: number } => r !== null);
		queueMicrotask(() => this.dispatchEvent(new Event("updateend")));
	}
}
```

Add to `FakeElement`:

```ts
	/** Test helper: a user seek. Real elements fire `seeking` synchronously
	 *  when currentTime is assigned. */
	seek(t: number): void {
		this.currentTime = t;
		this.dispatchEvent(new Event("seeking"));
	}
```

Existing tests that read `buffered` through a single range keep working because `length` is 1 after sequential appends.

- [ ] **Step 2: Write the failing tests**

```ts
	describe("seeking", () => {
		it("measures buffered-ahead from the range containing the playhead, not the last range", async () => {
			// 10 fragments of 10s. Play to 25s (buffer reaches ~55s), seek to
			// 90s. The loop must not wait on the stale 55s range.
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const before = fetchRange.mock.calls.length;

			element.seek(90);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls.length).toBeGreaterThan(before);
		});

		it("restarts the loop one fragment before the seek target", async () => {
			const fragments = Array.from({ length: 10 }, (_, i) => ({ start: i * 100, end: i * 100 + 99 }));
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { fragments, sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			fetchRange.mockClear();

			element.seek(65);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			// init, then fragment 5 (starts at 50s, one before the 60s fragment).
			expect(fetchRange.mock.calls[0]).toEqual([expect.any(String), 0, 710]);
			expect(fetchRange.mock.calls[1]).toEqual([expect.any(String), 500, 599]);
		});

		it("refetches evicted media when the user seeks back", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			for (const t of [10, 20, 30, 40, 50, 60, 70]) {
				element.advanceTime(t);
				await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			}
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			expect(sb.removeCalls.length).toBeGreaterThan(0);
			fetchRange.mockClear();

			element.seek(0);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls[0]).toEqual([expect.any(String), 0, 710]);
			expect(fetchRange.mock.calls[1][1]).toBe(1000); // fragment 0
		});

		it("does nothing when the target is already buffered", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			const before = fetchRange.mock.calls.length;

			element.seek(5);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls.length).toBe(before);
		});

		it("seeks into an appended next track by rebuilding from that track's fragments", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100, fragments: Array.from({ length: 10 }, (_, i) => ({ start: 5000 + i * 100, end: 5000 + i * 100 + 99 })) }));
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			fetchRange.mockClear();

			element.seek(100 + 45); // 45s into track b
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(fetchRange.mock.calls[1]).toEqual([expect.any(String), 5300, 5399]); // b, fragment 3
		});
	});
```

- [ ] **Step 3: Run to verify they fail**

Run: `./node_modules/.bin/vitest run src/lib/playback/mseEngine.test.ts -t seeking`
Expected: all five FAIL (no `seeking` listener; `bufferedAhead` reads the last range).

- [ ] **Step 4: Implement**

```ts
/** Backend fragment length (`FragmentDurationMicros`). Fragments start on a
 *  packet boundary, so fragment k begins at k*FRAGMENT_SECONDS plus up to
 *  one codec frame; seeks start one fragment early to cover that. */
export const FRAGMENT_SECONDS = 10;

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

	function isBuffered(t: number): boolean {
		const sb = sourceBuffer;
		if (!sb) return false;
		for (let i = 0; i < sb.buffered.length; i++) {
			if (sb.buffered.start(i) - 0.25 <= t && t < sb.buffered.end(i)) return true;
		}
		return false;
	}

	/** Manifests are kept per placed track so a seek can rebuild jobs. */
	const manifests = new Map<string, GaplessManifest>();

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
```

- `enqueueJob(track, manifest)` additionally does `manifests.set(track.trackId, manifest)`.
- `load()` clears `manifests` alongside `placed`/`jobs`.
- Register `element.addEventListener("seeking", onSeeking)` next to the `timeupdate` registration, and remove it in `teardown()`.
- In `runLoop`, the `continue` after `job.initAppended = true` already re-enters with the same job; no change there.

Note `stop(currentStop)` resolves the old token's promise, which releases any `waitFor` the old loop is parked on; that loop then observes `token.stopped` and exits before appending. Backpressure waits (`while (bufferedAhead() > LEAD_SECONDS)`) use the new `bufferedAhead`, so after a seek into a hole they read 0 and proceed.

- [ ] **Step 5: Run the engine suite, mutation check, typecheck**

Mutations, one at a time, each must fail at least one seeking test: (a) restore `bufferedAhead` to `buffered.end(length-1) - currentTime`; (b) delete the `- 1` in `fromFragment`; (c) remove the `seeking` listener.

- [ ] **Step 6: Propose commit**

```
Handle seeks in the MSE engine

- `bufferedAhead` read the last buffered range; after a forward seek
  that is stale data far ahead and the loop waited on a stalled element
- no `seeking` listener: a seek into evicted or unbuffered media stalled
  forever
- rebuild the job list from one fragment before the target
```

---

### Task 5: Retract an appended next track when the queue changes

**Files:**
- Modify: `frontend/src/lib/playback/types.ts` (`PlaybackEngine`), `frontend/src/lib/playback/mseEngine.ts`, `frontend/src/lib/playback/elementPairEngine.ts`, `frontend/src/components/MusicPlayer.tsx:1105-1125` (facade), `frontend/src/contexts/AudioPlayerContext.tsx:89-100` and the preload effect
- Test: `frontend/src/lib/playback/mseEngine.test.ts`, `frontend/src/contexts/AudioPlayerContext.test.tsx`

**Interfaces:**
- Produces: `PlaybackEngine.discardNext(): void` — drops every placed track after the one under the playhead, removes their media from the buffer, and stops appending them. Element-pair engine: no-op (its standby is overwritten by the next `prepareStandby`). Facade: `discardNext: () => activeTimelineEngine()?.discardNext()`. `ContextEngine` picks it.

- [ ] **Step 1: Engine test**

```ts
	describe("discardNext", () => {
		it("removes the appended track's media and stops appending it", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100 }));
			// Play to 80s so the loop has appended into b (offset 100s).
			for (const t of [10, 20, 30, 40, 50, 60, 70, 80]) {
				element.advanceTime(t);
				await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			}
			const sb = (element.srcObject as FakeMediaSource).sourceBuffers[0];
			expect(sb.ranges.at(-1)!.end).toBeGreaterThan(100);
			fetchRange.mockClear();

			engine.discardNext();
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);

			expect(sb.removeCalls.at(-1)).toEqual({ start: 100, end: Number.POSITIVE_INFINITY });
			expect(sb.ranges.at(-1)!.end).toBeLessThanOrEqual(100);
			element.advanceTime(90);
			await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			for (const call of fetchRange.mock.calls) {
				expect(call[0]).not.toContain("version_id=2");
			}
			expect(engine.getTrackDuration()).toBe(100);
		});

		it("re-appends a different next track after a discard", async () => {
			const { element, fetchRange, engine } = makeEngine({ secondsPerAppend: 10 });
			await engine.load(playableTrack("a", 1, { sampleCount: 44100 * 100 }));
			await engine.prepareNext(playableTrack("b", 2, { sampleCount: 44100 * 100 }));
			engine.discardNext();
			await engine.prepareNext(playableTrack("c", 3, { sampleCount: 44100 * 100, url: "/api/stream/c/gapless/alac?version_id=3" }));
			for (const t of [10, 20, 30, 40, 50, 60, 70, 80, 90]) {
				element.advanceTime(t);
				await flushUntilQuiescent(() => fetchRange.mock.calls.length);
			}
			const urls = fetchRange.mock.calls.map((c) => c[0]);
			expect(urls.some((u) => u.includes("version_id=3"))).toBe(true);
			element.advanceTime(105);
			expect(engine.getTrackTime()).toBeCloseTo(5, 3);
		});
	});
```

(`playableTrack`'s URL comes from `manifest().url`; pass `url` in overrides for track c so the fetch URLs are distinguishable. Track b uses the default URL, which carries `version_id=1`; adjust the `not.toContain` assertion to the URL you give b.)

- [ ] **Step 2: Context test**

```tsx
	it("retracts an appended next track when the queue changes before the boundary", async () => {
		const engine = await runPreload({ gapless: true, canAppend: true });
		expect(engine.prepareNext).toHaveBeenCalledTimes(1);

		// The user drops t2 from the queue; t3 becomes next.
		await act(async () => {
			ctx.removeFromQueue(0);
		});
		await act(async () => {
			await Promise.resolve();
			await Promise.resolve();
		});

		expect(engine.discardNext).toHaveBeenCalledTimes(1);
		expect(engine.prepareNext).toHaveBeenCalledTimes(2);
		expect(engine.prepareNext.mock.calls[1][0]).toEqual(expect.objectContaining({ trackId: "t3" }));
	});
```

Add `discardNext: vi.fn()` to `fakeEngine`. Check how `runPreload` builds the queue: `ctx.play(TRACKS[0], TRACKS, true)` sets `currentProjectTracks`, not `queue`; if `removeFromQueue(0)` is a no-op in that setup, use `ctx.addToQueue(TRACKS[2])` before the preload instead (queue `[t3]` makes t3 next), then `ctx.removeFromQueue(0)` makes t2 (from project order) next. Assert the second `prepareNext` is for whichever track the setup makes next. The point is one discard, then one new append.

- [ ] **Step 3: Run to verify both fail**

Expected: engine test FAILS — `discardNext is not a function`. Context test FAILS the same way.

- [ ] **Step 4: Implement**

`types.ts`:

```ts
	/** Drop every track placed after the one under the playhead, and its
	 *  media. The preload path calls this when the queue changes after an
	 *  append so the boundary does not run into a track that is no longer
	 *  next. No-op when nothing is appended. */
	discardNext(): void;
```

`mseEngine.ts`:

```ts
	function discardNext(): void {
		if (!sourceBuffer || currentStop.stopped) return;
		const pos = currentPosition() ?? (placed[0] ? { track: placed[0], trackTime: 0 } : null);
		if (!pos) return;
		const idx = placed.indexOf(pos.track);
		if (idx === placed.length - 1) return;

		const keep = placed[idx];
		const end = keep.offsetSeconds + durationSeconds(keep);
		for (const dropped of placed.slice(idx + 1)) manifests.delete(dropped.trackId);
		placed = placed.slice(0, idx + 1);

		stop(currentStop);
		const token = createStopToken();
		currentStop = token;
		// The current track's own job restarts from where the playhead is;
		// anything already buffered up to `end` stays, the loop skips
		// appends the buffer already covers only by backpressure, so start
		// at the fragment after the buffered end to avoid re-appending.
		const ahead = bufferedAhead();
		const resumeFrom = Math.floor((pos.trackTime + ahead) / FRAGMENT_SECONDS);
		jobs = [jobFor(keep, resumeFrom)];

		const sb = sourceBuffer;
		const done = waitFor(sb, "updateend", token);
		sb.remove(end, Number.POSITIVE_INFINITY);
		void done.then(() => runLoop(token));
	}
```

`elementPairEngine.ts`: `discardNext() {}` with a one-line comment: the standby is simply overwritten by the next `prepareStandby`. Add it to the returned object and to the `ElementPairEngine` interface if it extends `PlaybackEngine`.

MusicPlayer facade: `discardNext: () => activeTimelineEngine()?.discardNext(),`.

`AudioPlayerContext.tsx`:

- Add `| "discardNext"` to the `ContextEngine` pick.
- Add `const appendedNextIdRef = useRef<string | null>(null);` next to `preloadKeyRef`.
- In the preload effect, right after computing `next` and before the key check:

```ts
    // A track was appended to the live timeline for a next that is no longer
    // next (queue reorder/remove, shuffle toggle). Retract it before anything
    // else, or the boundary plays the stale track.
    if (appendedNextIdRef.current && appendedNextIdRef.current !== next.id) {
      getEngine()?.discardNext();
      appendedNextIdRef.current = null;
      preloadKeyRef.current = null;
    }
```

- Where `prepareNext` is awaited: set `appendedNextIdRef.current = next.id;` immediately **before** the `await engine?.prepareNext(playable)` (the append is enqueued synchronously inside `prepareNext`, so mark it before the await, not after a `cancelled` check).
- In the `[currentTrack?.id]` effect that clears the preload, also `appendedNextIdRef.current = null;` (the appended track is now the current one).
- In the `!next` branch, if `appendedNextIdRef.current` is set, call `getEngine()?.discardNext()` and clear it (the queue emptied).

- [ ] **Step 5: Run everything, typecheck, mutation check**

Mutation: make `discardNext` in the context branch a no-op. The context test must fail. Mutation: remove the `sb.remove(end, ...)` call. The first engine test must fail.

- [ ] **Step 6: Propose commit**

```
Retract an appended next track when the queue changes

- `prepareNext` could only add; a reorder/remove after the append left
  the stale track at the boundary
- `discardNext` truncates the timeline after the current track and
  removes its media
```

---

### Task 6: Fix the MSE element unlock

**Files:**
- Modify: `frontend/src/components/MusicPlayer.tsx:473-504` (`unlockMseElement`)
- Test: `frontend/src/components/MusicPlayer.engineSelection.test.tsx`

- [ ] **Step 1: Write the failing tests**

```tsx
	it("never touches the MSE element's srcObject once an engine owns it", async () => {
		codecSupport.supported = ["alac"];
		store.set({
			currentPlayable: { trackId: "t1", versionId: null, url: TRACK_A_URL, manifest: MANIFEST },
		});
		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});
		const { elMse } = audioElements(container);
		const live = {};
		Object.defineProperty(elMse, "srcObject", { value: live, writable: true, configurable: true });

		await act(async () => {
			document.dispatchEvent(new Event("pointerdown"));
		});

		expect(elMse.srcObject).toBe(live);
	});

	it("marks the MSE element unlocked at the play() call and detaches the gesture listener", async () => {
		const play = vi.fn(() => new Promise<void>(() => {})); // never settles, like a sourceless MediaSource
		HTMLMediaElement.prototype.play = play;
		const { container } = render(<MusicPlayer hideControls />);
		await act(async () => {});
		const { elMse } = audioElements(container);
		Object.defineProperty(elMse, "srcObject", { value: null, writable: true, configurable: true });

		await act(async () => {
			document.dispatchEvent(new Event("pointerdown"));
		});
		const callsAfterFirst = play.mock.calls.length;
		await act(async () => {
			document.dispatchEvent(new Event("pointerdown"));
		});

		expect(callsAfterFirst).toBeGreaterThan(0);
		expect(play.mock.calls.length).toBe(callsAfterFirst);
	});
```

(The second assertion depends on `engine.isStandbyUnlocked()` also being true so the listener detaches; if the standby unlock also calls `play()` the count includes it. Assert on the MSE element specifically by spying `elMse.play` instead of the prototype if needed.)

- [ ] **Step 2: Run to verify they fail**

Expected: first FAILS (srcObject replaced), second FAILS (play called again).

- [ ] **Step 3: Implement**

```ts
  const unlockMseElement = useCallback(() => {
    if (mseUnlockedRef.current || mseUnlockInFlightRef.current) return;
    const el = mseElRef.current;
    if (!el) return;
    // The engine owns the element from its first load() onward and plays it
    // inside the user's own tap. Attaching anything here would replace the
    // live timeline.
    if (mseEngineRef.current) return;

    const w = window as unknown as {
      ManagedMediaSource?: typeof MediaSource;
      MediaSource?: typeof MediaSource;
    };
    const MediaSourceImpl = w.ManagedMediaSource ?? w.MediaSource;
    if (!MediaSourceImpl) return;

    mseUnlockInFlightRef.current = true;
    const wasMuted = el.muted;
    el.muted = true;
    const throwaway = new MediaSourceImpl();
    (el as unknown as { srcObject: unknown }).srcObject = throwaway;

    // iOS registers the gesture at the play() CALL. The promise itself can
    // never resolve — a MediaSource with no SourceBuffer has nothing to play
    // — so waiting on it is what kept this unlock from ever "succeeding".
    const pending = el.play();
    mseUnlockedRef.current = true;

    pending
      .catch(() => {
        // AbortError when the engine takes the element over, or a sourceless
        // play that the browser rejects. Either way the gesture was consumed.
      })
      .finally(() => {
        el.muted = wasMuted;
        mseUnlockInFlightRef.current = false;
        // Only detach our own throwaway. If the engine has attached its
        // MediaSource in the meantime, leave it.
        const current = (el as unknown as { srcObject: unknown }).srcObject;
        if (current === throwaway && !mseEngineRef.current) {
          el.pause();
          (el as unknown as { srcObject: unknown }).srcObject = null;
        }
      });
  }, []);
```

- [ ] **Step 4: Run suites, mutation check**

Mutation: delete `if (mseEngineRef.current) return;`. First test must fail.

- [ ] **Step 5: Propose commit**

```
Stop the MSE unlock from clobbering a live timeline

- unlock waited on a play() that cannot resolve on a sourceless
  MediaSource, so it never marked unlocked and re-fired on every gesture
- skip when the engine owns the element; mark unlocked at the call
```

---

### Task 7: Verify in Chrome against the deployed backend

Chrome supports `audio/mp4; codecs="flac"` through MSE, so the FLAC segment sets exercise the whole engine on this Mac. This is the first step that can show whether the seam is gone. Nothing in Tasks 1–6 counts as verified until this passes.

**Files:**
- Modify: `frontend/src/components/MusicPlayer.tsx` — opt-in boundary logging (kept; it is cheap and the last session proved static reading was not enough)

- [ ] **Step 1: Opt-in boundary diagnostics**

In `getMseEngine`, after `created.subscribe({...})`, and in the transport-listener effect, add logging gated on `localStorage.getItem("gaplessDebug") === "1"`:

```ts
const gaplessDebug = () => {
  try {
    return localStorage.getItem("gaplessDebug") === "1";
  } catch {
    return false;
  }
};
```

- In the `trackchange` handler: `if (gaplessDebug()) console.info("[gapless] trackchange", { trackId, elementTime: mseElRef.current?.currentTime, buffered: rangesOf(mseElRef.current) });`
- In the listener effect, when `desiredEngine === "mse"`, also listen for `waiting`, `stalled`, and `seeking` on `audio` and log the same shape with the event type.
- `rangesOf(el)` returns `Array.from({length: el.buffered.length}, (_, i) => [el.buffered.start(i), el.buffered.end(i)])`.

Also log, in the context's preload effect, `{ append, engineKind: engine?.kind, nextEngineKind, nextTrackId: next.id }` under the same gate.

- [ ] **Step 2: Start the dev server**

Find the Unraid IP per the `home-networking-tasks` skill (it also defines the SSH logging protocol if a shell on Unraid is needed). Then from `frontend/`:

```bash
VITE_PROXY_TARGET=http://<unraid-ip>:8081 ./node_modules/.bin/vite --port 3000
```

- [ ] **Step 3: Confirm the deployed backend offers manifests**

Sign in at `http://localhost:3000` (the user's own account; use the password-manager flow or let the user sign in by hand). In DevTools console:

```js
localStorage.setItem("gaplessDebug", "1");
MediaSource.isTypeSupported('audio/mp4; codecs="flac"'); // must be true
```

Then from the Network tab, the `GET /api/media/stream/<id>?codecs=alac,flac` (no `quality`) response for a lossless track must carry a `gapless` object with `codec: "flac"`. If it carries none: check the user's `default_quality` on the profile page is `source`, and that the version has a completed FLAC segment set (`/api/stats` exposes `commit_sha` for the deployed build, which is how to confirm `aef939c` or later is running).

- [ ] **Step 4: The boundary**

Queue two lossless tracks from one project. Play the first, seek to ~15s before its end, let it cross. Expected console sequence: one `preload` log with `append: true`, then exactly one `trackchange` with `buffered` showing a single contiguous range spanning the boundary, and **no** `waiting`/`stalled` between them. Listen.

Three outcomes:
- Seamless and no `waiting`: the gap is fixed. Go to Step 5.
- `waiting` fires at the boundary with a hole in `buffered` around the offset: the gap is a buffer discontinuity between track A's real media duration and `sampleCount/sampleRate`. Record the hole size. This is a backend question (fragment tail vs. declared sample count), not a client one; stop and report with the numbers.
- `append: false` or no `trackchange`: engine was never used. Read the preload log's `engineKind`/`nextEngineKind` and the stream responses; this is the Task 1/3 territory and the logs say which.

- [ ] **Step 5: Scrubbing, queue edits, pause/resume**

With `gaplessDebug` on, during a lossless track:
- Seek back to 0 after >40s of playback: `seeking` log, fetches resume, audio resumes within a second. (Task 4.)
- Seek forward past the buffered end: same. (Task 4.)
- In the preload window, remove the next track from the queue: engine `discardNext` fires (add a debug log there) and a new `append: true` for the new next. Boundary goes to the right track. (Task 5.)
- Pause in the preload window, wait 10s, resume: next track still crosses gapless (no discard fired).
- Volume slider and scroll-wheel volume during MSE playback: audio continues, level changes. (The upstream wheel handler writes `volumePercentage`; MusicPlayer's volume effect writes `audioRef.current.volume`, which is the MSE element under MSE.)
- Lossless → MP3 boundary: today's seam, audio continues, no silence.

- [ ] **Step 6: Full suites**

`./node_modules/.bin/tsc --noEmit -p .` and `./node_modules/.bin/vitest run` — both clean. `go build ./... && go test ./...` from the repo root — unchanged, still clean.

- [ ] **Step 7: Propose commit**

```
Add opt-in gapless boundary diagnostics

- gated on localStorage.gaplessDebug; logs trackchange, waiting,
  stalled, seeking, and the preload append decision
```

---

### Task 8: Device checks and deploy

Only after Task 7 passes in Chrome.

- [ ] **Step 1: Merge and deploy**

Push the branch; merge PR #6 into `main` on the fork (user's call). Rebuild on Unraid per the handoff: `docker compose build --no-cache` then `up -d --force-recreate`, and confirm the image ID changed and `/api/stats` reports the new `commit_sha`. Every SSH command follows `references/ssh-protocol.md` in the `home-networking-tasks` skill.

- [ ] **Step 2: Run the six device checks on the iPhone**

From `docs/superpowers/plans/2026-08-25-gapless-playback-device-checklist.md` and the client handoff. Record each as pass/fail with what was observed, in that file:

1. All-lossless album, no audible seam (ALAC on Safari).
2. Lossless → MP3 boundary degrades to a seam, not silence.
3. First lossless track after a cold start plays (Task 6 is what this tests).
4. Lock screen shows the right track across a boundary.
5. AirPlay via Control Center keeps working across a transition.
6. Scrubbing lands where expected; waveform comments land at the right moment.

- [ ] **Step 3: Propose commit**

```
Record device check results for gapless lossless playback
```

---

## Deliberately not in this plan

- **The "Maximum update depth exceeded" report.** Not reproduced this session; the stash's mitigation was a no-op. If it recurs with `gaplessDebug` on, the React component stack in the error names the effect, and that is the starting point, not a guess.
- **`package-lock.json` out of sync** with `package.json`. Separate change.
- **Backend tail-duration question** (fragment media length vs. `sampleCount`). Only if Task 7 Step 4 shows a hole in `buffered` at the boundary.
- **`discardNext` on the context when `loopMode` flips to `track`.** The preload effect returns early under loop-track and leaves an appended next in the timeline. The MSE `ended` handler under loop-track seeks to 0 and replays, so the appended track never plays; it just sits in the buffer. Harmless, note it in followups.

## Self-review

- Findings 1–7 each map to a task (1→T1, 2→T3, 3,4→T4, 5→T5, 6→T6, 7→T2). Device checks → T8. Verification before claiming done → T7.
- `discardNext` is named identically in `types.ts`, `mseEngine.ts`, `elementPairEngine.ts`, the facade, `ContextEngine`, the context effect, and both tests. `FRAGMENT_SECONDS` is defined in T4 and used in T5. `jobFor`, `manifests`, `isBuffered`, `bufferedAhead` are defined in T4 and used in T5. `appendedNextIdRef` is defined and used only in T5. `setMseElement` is defined in T2 only.
- Every code step has code. Every test step has the test, the run command, and the expected failure.
