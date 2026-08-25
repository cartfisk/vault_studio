# Near-Gapless Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the audible gap between tracks by preloading the next track into a second `<audio>` element at ~20s remaining and swapping to it on `ended`.

**Architecture:** `AudioPlayerContext` owns queue/URL policy and publishes a freshly signed URL for the next track when the current one nears its end. `MusicPlayer` owns two `<audio>` elements, buffers that URL into the idle one, and swaps active/standby on transition. All decidable logic lives in a pure `src/lib/gaplessPreload.ts` module so it can be unit-tested without a DOM.

**Tech Stack:** React 19, TypeScript, Vite 7, Vitest 3 (default `node` environment), Bun.

## Global Constraints

- Target seam: ~20-80ms. True sample-accurate gapless is explicitly out of scope — see spec section "Deferred".
- Scope is `MusicPlayer` and `AudioPlayerContext` only. Do **not** modify `SharedTrackPlayer` (single track, no queue).
- Every failure path must degrade to the existing `src`/`load()`/`canplay` sequence. Gapless is an optimization, never a dependency.
- Both `<audio>` elements must carry identical attributes: `preload="auto"`, `crossOrigin="anonymous"`, `playsInline`.
- Preserve the existing auth gate: only mint stream URLs when `!!shareTokenRef.current || isAuthenticated`.
- `PRELOAD_LEAD_SECONDS = 20`.
- `SIGNED_URL_TTL_SECONDS = 300` — mirrors the server's `SIGNED_URL_TTL=5m` (docker-compose.yml:18). This is a duplicated constant across the client/server boundary; the 30s safety margin in `isPreloadStale` absorbs drift. If the server default changes, this must change too.
- Tests run in Vitest's default `node` environment. Vitest reads `frontend/vite.config.ts`, which provides the `@` -> `./src` alias. Do not add a jsdom environment — keep all new logic DOM-free and unit-testable.
- Run all commands from `frontend/`. Install deps first with `bun install` if `node_modules` is absent.

## File Structure

**Create:**
- `frontend/src/lib/gaplessPreload.ts` — pure decision logic. No DOM, no React, no network. Owns `shouldStartPreload`, `isPreloadStale`, `chooseTransition`, and the shared constants.
- `frontend/src/lib/gaplessPreload.test.ts` — unit tests for the above.
- `docs/superpowers/plans/2026-08-25-gapless-playback-device-checklist.md` — manual iOS verification checklist.

**Modify:**
- `frontend/src/contexts/AudioPlayerContext.tsx` — delete the dead detached-`Audio` preload path; add `nextTrackPreload` policy.
- `frontend/src/components/MusicPlayer.tsx` — dual `<audio>` elements, standby buffering, swap, iOS unlock.

---

### Task 1: Pure preload decision module

**Files:**
- Create: `frontend/src/lib/gaplessPreload.ts`
- Test: `frontend/src/lib/gaplessPreload.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `PRELOAD_LEAD_SECONDS: number` (20)
  - `SIGNED_URL_TTL_SECONDS: number` (300)
  - `SWAP_MIN_READY_STATE: number` (3)
  - `SILENT_AUDIO_DATA_URI: string`
  - `shouldStartPreload(input: { currentTime: number; duration: number; leadSeconds?: number }): boolean`
  - `isPreloadStale(input: { signedAt: number; now: number; ttlSeconds?: number; safetyMarginSeconds?: number }): boolean`
  - `chooseTransition(input: { standbySrc: string | null; targetUrl: string; readyState: number }): "swap" | "load"`

- [ ] **Step 1: Write the failing test**

Create `frontend/src/lib/gaplessPreload.test.ts`:

```ts
import { describe, expect, it } from "vitest";

import {
  chooseTransition,
  isPreloadStale,
  PRELOAD_LEAD_SECONDS,
  shouldStartPreload,
  SIGNED_URL_TTL_SECONDS,
  SWAP_MIN_READY_STATE,
} from "@/lib/gaplessPreload";

describe("shouldStartPreload", () => {
  it("fires once inside the lead window", () => {
    expect(shouldStartPreload({ currentTime: 100, duration: 240 })).toBe(false);
    expect(shouldStartPreload({ currentTime: 219, duration: 240 })).toBe(false);
    expect(shouldStartPreload({ currentTime: 220, duration: 240 })).toBe(true);
    expect(shouldStartPreload({ currentTime: 239, duration: 240 })).toBe(true);
  });

  it("fires immediately for tracks shorter than the lead window", () => {
    expect(shouldStartPreload({ currentTime: 0, duration: 12 })).toBe(true);
  });

  it("fires on a forward seek into the window", () => {
    expect(shouldStartPreload({ currentTime: 235, duration: 240 })).toBe(true);
  });

  it("stops firing after a backward seek out of the window", () => {
    expect(shouldStartPreload({ currentTime: 30, duration: 240 })).toBe(false);
  });

  it("refuses to decide without a usable duration", () => {
    expect(shouldStartPreload({ currentTime: 5, duration: 0 })).toBe(false);
    expect(shouldStartPreload({ currentTime: 5, duration: Number.NaN })).toBe(false);
    expect(shouldStartPreload({ currentTime: 5, duration: Infinity })).toBe(false);
  });

  it("refuses to decide without a usable currentTime", () => {
    expect(shouldStartPreload({ currentTime: Number.NaN, duration: 240 })).toBe(false);
    expect(shouldStartPreload({ currentTime: -1, duration: 240 })).toBe(false);
  });

  it("honours a custom lead window", () => {
    expect(shouldStartPreload({ currentTime: 200, duration: 240, leadSeconds: 60 })).toBe(true);
    expect(shouldStartPreload({ currentTime: 100, duration: 240, leadSeconds: 60 })).toBe(false);
  });

  // Pins the tuning value the rest of this suite's expectations are computed
  // from. If someone retunes the lead window, these expectations must be
  // recomputed deliberately rather than silently drifting.
  it("defaults the lead window to 20 seconds", () => {
    expect(PRELOAD_LEAD_SECONDS).toBe(20);
  });
});

describe("isPreloadStale", () => {
  const signedAt = 1_000_000;

  it("treats a freshly signed url as usable", () => {
    expect(isPreloadStale({ signedAt, now: signedAt + 5_000 })).toBe(false);
  });

  it("treats a url past its ttl as stale", () => {
    expect(isPreloadStale({ signedAt, now: signedAt + 301_000 })).toBe(true);
  });

  it("treats a url inside the safety margin as stale before it actually expires", () => {
    expect(isPreloadStale({ signedAt, now: signedAt + 275_000 })).toBe(true);
    expect(isPreloadStale({ signedAt, now: signedAt + 265_000 })).toBe(false);
  });

  // This constant duplicates the server's SIGNED_URL_TTL across the
  // client/server boundary. Pinning it means a silent edit on either side
  // breaks a test instead of producing 403s at swap time in production.
  it("mirrors the server ttl", () => {
    expect(SIGNED_URL_TTL_SECONDS).toBe(300);
  });
});

describe("chooseTransition", () => {
  const targetUrl = "https://example.test/stream/abc";

  it("swaps when the standby holds the target and is buffered enough", () => {
    expect(chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: SWAP_MIN_READY_STATE })).toBe("swap");
    expect(chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: 4 })).toBe("swap");
  });

  it("loads when the standby is empty", () => {
    expect(chooseTransition({ standbySrc: null, targetUrl, readyState: 4 })).toBe("load");
    expect(chooseTransition({ standbySrc: "", targetUrl, readyState: 4 })).toBe("load");
  });

  it("loads when the standby holds a different url", () => {
    expect(chooseTransition({ standbySrc: "https://example.test/stream/other", targetUrl, readyState: 4 })).toBe("load");
  });

  it("loads when the standby has not buffered enough", () => {
    expect(chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: 2 })).toBe("load");
    expect(chooseTransition({ standbySrc: targetUrl, targetUrl, readyState: 0 })).toBe("load");
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd frontend && bun install && bun run test -- gaplessPreload
```

Expected: FAIL — `Failed to resolve import "@/lib/gaplessPreload"`.

- [ ] **Step 3: Write minimal implementation**

Create `frontend/src/lib/gaplessPreload.ts`:

```ts
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
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd frontend && bun run test -- gaplessPreload
```

Expected: PASS, 16 tests.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/gaplessPreload.ts frontend/src/lib/gaplessPreload.test.ts
git commit -m "Add pure preload decision module for gapless playback"
```

---

### Task 2: Delete the dead detached-Audio preload path

The existing preload buffers into a detached `new Audio()`. A detached element cannot hand its buffer to a rendered `<audio>`, so `play()` reuses only the URL string and the real element refetches. Removing it first keeps later tasks readable.

**Expected transient behavior:** after this task, transitions use the plain load path with no HTTP-cache warming. Slightly slower than today until Task 4 lands. This is intentional.

**Files:**
- Modify: `frontend/src/contexts/AudioPlayerContext.tsx`
- Modify: `frontend/src/components/MusicPlayer.tsx`
- Modify: `frontend/src/components/TrackCard.tsx:231`
- Modify: `frontend/src/components/modals/TrackVersionsModal.tsx:459`

**Interfaces:**
- Consumes: nothing.
- Produces: `AudioPlayerContextType` no longer has `getPreloadedAudio` or `clearPreloadedAudio`, and `play()` loses its `forceReload` parameter. New signature:
  `play(track: Track, projectTracks?: Track[], autoPlay?: boolean, queue?: Track[]) => void`

- [ ] **Step 1: Remove the context interface members**

In `frontend/src/contexts/AudioPlayerContext.tsx`, delete these two lines from `AudioPlayerContextType` (around lines 84-85):

```ts
  getPreloadedAudio: () => HTMLAudioElement | null;
  clearPreloadedAudio: () => void;
```

- [ ] **Step 2: Remove the refs**

Delete these two lines (around lines 127-128):

```ts
  const preloadedTrackIdRef = useRef<string | null>(null);
  const preloadAudioRef = useRef<HTMLAudioElement | null>(null);
```

Also delete (around line 130):

```ts
  const preloadRequestIdRef = useRef(0);
```

- [ ] **Step 3: Delete `preloadNextTrack` and its effects**

Delete the entire `const preloadNextTrack = useCallback(...)` block (starts around line 303, ends at `}, [getNextTrack]);` around line 410).

Delete the effect immediately following it that calls `preloadNextTrack` on a 500ms timer (around lines 412-423):

```ts
  useEffect(() => {
    if (currentTrack && isPlaying) {
      const timer = setTimeout(() => {
        preloadNextTrack();
      }, 500);
      return () => clearTimeout(timer);
    }
  }, [ ... ]);
```

Delete the unmount-cleanup effect that nulls `preloadAudioRef` (around lines 425-434).

- [ ] **Step 4: Remove the preloaded branch from `play()`**

In `play()`, delete this block (around lines 472-498):

```ts
      const isPreloaded =
        preloadedTrackIdRef.current === track.id && preloadAudioRef.current;

      const preloadedStreamUrl =
        isPreloaded && !forceReload ? preloadAudioRef.current?.src : null;

      if (isPreloaded && !forceReload) {
        preloadedTrackIdRef.current = null;
        preloadAudioRef.current = null;
      }

      if (preloadedStreamUrl) {
        setCurrentTrack(track);
        setAudioUrl(preloadedStreamUrl);
        setIsPlaying(autoPlay);

        void ensureTrackWaveform(track).then((trackWithWaveform) => {
          if (playRequestIdRef.current !== requestId) return;
          setCurrentTrack((activeTrack) =>
            activeTrack?.id === track.id ? trackWithWaveform : activeTrack,
          );
        });

        setTimeout(() => preloadNextTrack(), 100);
        return;
      }
```

Delete the trailing `setTimeout(() => preloadNextTrack(), 100);` at the end of `play()` (around line 533).

Update the `play` dependency array (around line 535) from:

```ts
    [preloadNextTrack, isShuffled, ensureTrackWaveform, cacheWaveforms],
```

to:

```ts
    [isShuffled, ensureTrackWaveform, cacheWaveforms],
```

- [ ] **Step 4a: Remove the now-dead `forceReload` parameter**

`forceReload` existed only to bypass the preload cache deleted above. It now has
no reader, and `tsconfig.json` sets `"noUnusedParameters": true`, so leaving it
fails the `tsc` gate in Step 9 with `TS6133`.

Remove it from the `AudioPlayerContextType` signature (around line 59):

```ts
    forceReload?: boolean,
```

Remove it from the `play` implementation signature (around line 440):

```ts
      forceReload: boolean = false,
```

Update all three call sites, dropping the fourth positional argument:

`frontend/src/contexts/AudioPlayerContext.tsx:779`

```ts
        play(firstTrack, undefined, false, rest);
```

`frontend/src/components/TrackCard.tsx:231`

```ts
    play(trackData, [trackData], true, []);
```

`frontend/src/components/modals/TrackVersionsModal.tsx:459`

```ts
          play(updatedTrack, undefined, false);
```

The `TrackVersionsModal` caller passed `true` to force a reload when switching
between versions of the same track. That still works: `play()` mints a fresh
signed URL on every non-preload path, so the new version's URL differs from the
currently loaded one and the audio element reloads on its own.

- [ ] **Step 5: Delete the accessor callbacks**

Delete (around lines 823-830):

```ts
  const getPreloadedAudio = useCallback(() => {
    return preloadAudioRef.current;
  }, []);

  const clearPreloadedAudio = useCallback(() => {
    preloadedTrackIdRef.current = null;
    preloadAudioRef.current = null;
  }, []);
```

- [ ] **Step 6: Clean the sign-out effect**

In the `if (!isAuthenticated)` effect, delete (around lines 1063-1068):

```ts
      if (preloadAudioRef.current) {
        preloadAudioRef.current.pause();
        preloadAudioRef.current.src = "";
        preloadAudioRef.current = null;
      }
      preloadedTrackIdRef.current = null;
```

- [ ] **Step 7: Remove from the provider value**

Delete these two lines from the `value={{ ... }}` object (around lines 1125-1126):

```ts
        getPreloadedAudio,
        clearPreloadedAudio,
```

- [ ] **Step 8: Remove the consumer usage in MusicPlayer**

In `frontend/src/components/MusicPlayer.tsx`, delete from the `useAudioPlayer()` destructure (around lines 60-61):

```ts
    getPreloadedAudio,
    clearPreloadedAudio,
```

In the `audioUrl` effect, delete (around lines 464-469):

```ts
      const preloadedAudio = getPreloadedAudio();
      if (preloadedAudio && preloadedAudio.src === audioUrl) {
        clearPreloadedAudio();
      }
```

Update that effect's dependency array (around line 499) from:

```ts
  }, [audioUrl, getPreloadedAudio, clearPreloadedAudio]);
```

to:

```ts
  }, [audioUrl]);
```

- [ ] **Step 9: Verify it compiles and existing tests still pass**

```bash
cd frontend && bunx tsc --noEmit && bun run test
```

Expected: no TypeScript errors, all existing tests pass.

- [ ] **Step 10: Commit**

```bash
git add frontend/src/contexts/AudioPlayerContext.tsx frontend/src/components/MusicPlayer.tsx
git commit -m "Remove dead detached-Audio preload path"
```

---

### Task 3: Render two audio elements with an active/standby split

Pure refactor. Two elements exist, one is idle, behavior is unchanged.

**Files:**
- Modify: `frontend/src/components/MusicPlayer.tsx`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces (module-local, used by Task 4 and Task 5):
  - `elARef: React.RefObject<HTMLAudioElement | null>`
  - `elBRef: React.RefObject<HTMLAudioElement | null>`
  - `activeKey: "a" | "b"` state with `setActiveKey`
  - `audioRef` keeps its existing name, type, and object identity — it is now repointed rather than bound directly to JSX.

- [ ] **Step 1: Add the element refs and active key**

In `frontend/src/components/MusicPlayer.tsx`, replace this line (around line 113):

```ts
  const audioRef = useRef<HTMLAudioElement | null>(null);
```

with:

```ts
  // Two elements ping-pong so the next track can buffer while the current one
  // plays. `audioRef` keeps stable object identity and is repointed at
  // whichever element is currently active, so every existing `audioRef.current`
  // call site and `audioPlayerRef.current = { audio: audioRef }` keep working.
  const elARef = useRef<HTMLAudioElement | null>(null);
  const elBRef = useRef<HTMLAudioElement | null>(null);
  const [activeKey, setActiveKey] = useState<"a" | "b">("a");
  const audioRef = useRef<HTMLAudioElement | null>(null);
```

- [ ] **Step 2: Keep `audioRef` pointed at the active element**

Immediately after the refs above, add:

```ts
  const getElement = useCallback(
    (key: "a" | "b") => (key === "a" ? elARef.current : elBRef.current),
    [],
  );

  // Must run before any effect that reads audioRef.current.
  useEffect(() => {
    audioRef.current = getElement(activeKey);
  }, [activeKey, getElement]);
```

- [ ] **Step 3: Re-attach media listeners on swap**

Find the effect that attaches `play`/`pause`/`ended`/`loadedmetadata`/`timeupdate` listeners (starts around line 377 with `const audio = audioRef.current;`).

Change its dependency array (around line 420) from:

```ts
  }, [onPlayingChange, onEnded, onDurationChange, isDragging]);
```

to:

```ts
  }, [onPlayingChange, onEnded, onDurationChange, isDragging, activeKey]);
```

- [ ] **Step 4: Render both elements**

Replace the single `<audio>` element (around lines 839-847):

```tsx
      <audio
        ref={audioRef}
        // Use the native loop behaviour; custom loop attempts caused stutter
        // and play() interruption errors on some platforms.
        loop={loopMode === "track"}
        preload="auto"
        crossOrigin="anonymous"
        playsInline
      />
```

with:

```tsx
      {/*
        Attributes must stay identical between the two elements — attribute
        drift makes the standby's buffer unusable at swap time.
        Only the active element loops; a looping standby would never fire
        `ended` after a swap.
      */}
      <audio
        ref={elARef}
        // Use the native loop behaviour; custom loop attempts caused stutter
        // and play() interruption errors on some platforms.
        loop={loopMode === "track" && activeKey === "a"}
        preload="auto"
        crossOrigin="anonymous"
        playsInline
      />
      <audio
        ref={elBRef}
        loop={loopMode === "track" && activeKey === "b"}
        preload="auto"
        crossOrigin="anonymous"
        playsInline
      />
```

- [ ] **Step 5: Verify nothing regressed**

```bash
cd frontend && bunx tsc --noEmit && bun run test
```

Expected: no TypeScript errors, all existing tests pass.

Then run the app and confirm by hand that a track still plays, pauses, seeks, and advances to the next track:

```bash
cd frontend && bun run dev
```

- [ ] **Step 6: Commit**

```bash
git add frontend/src/components/MusicPlayer.tsx
git commit -m "Render active/standby audio element pair"
```

---

### Task 4: Publish and consume the next-track preload

**Files:**
- Modify: `frontend/src/contexts/AudioPlayerContext.tsx`
- Modify: `frontend/src/components/MusicPlayer.tsx`

**Interfaces:**
- Consumes: `shouldStartPreload`, `isPreloadStale`, `chooseTransition`, `SIGNED_URL_TTL_SECONDS` from Task 1; `elARef`, `elBRef`, `activeKey`, `setActiveKey`, `getElement` from Task 3. Restores `getNextTrack` (deleted in Task 2 as orphaned) in Step 2a.
- Produces: `AudioPlayerContextType` gains `nextTrackPreload: NextTrackPreload | null` where `interface NextTrackPreload { trackId: string; url: string; signedAt: number }`.

- [ ] **Step 1: Add the import and the published type**

In `frontend/src/contexts/AudioPlayerContext.tsx`, add to the imports:

```ts
import {
  isPreloadStale,
  shouldStartPreload,
} from "../lib/gaplessPreload";
```

Above `interface AudioPlayerContextType`, add:

```ts
export interface NextTrackPreload {
  trackId: string;
  url: string;
  /** Date.now() at signing time, used to detect expiry before swap. */
  signedAt: number;
}
```

Add to `AudioPlayerContextType`, next to the other read-only fields:

```ts
  nextTrackPreload: NextTrackPreload | null;
```

- [ ] **Step 2: Add the state and the re-arm guard**

Alongside the other `useState` calls in the provider, add:

```ts
  const [nextTrackPreload, setNextTrackPreload] =
    useState<NextTrackPreload | null>(null);
  /**
   * `${currentTrackId}:${nextTrackId}:${quality}` for the preload currently in
   * flight or published. Keyed on the tuple rather than a boolean so a queue
   * reorder, shuffle toggle, or quality change re-arms the trigger instead of
   * latching it off.
   */
  const preloadKeyRef = useRef<string | null>(null);
```

- [ ] **Step 2a: Restore the `getNextTrack` selector**

Task 2 deleted this selector because removing `preloadNextTrack` left it with no
consumer and `tsconfig.json` sets `"noUnusedLocals": true`. Step 3 below is its
new consumer, so restore it verbatim. Do **not** rewrite it from scratch — it
mirrors the selection order in `nextTrack()` (queue first, then project tracks,
honouring shuffle) and divergence here means preloading the wrong track.

Place it immediately above the preload trigger effect:

```ts
  const getNextTrack = useCallback((): Track | null => {
    if (!currentTrack) return null;

    if (queue.length > 0) {
      return queue[0];
    }

    if (currentProjectTracks.length > 0) {
      if (isShuffled && shuffledProjectTracks.length > 0) {
        const currentIndex = shuffledProjectTracks.findIndex(
          (t) => t.id === currentTrack.id,
        );
        if (
          currentIndex !== -1 &&
          currentIndex < shuffledProjectTracks.length - 1
        ) {
          return shuffledProjectTracks[currentIndex + 1];
        }
      } else {
        const currentIndex = currentProjectTracks.findIndex(
          (t) => t.id === currentTrack.id,
        );
        if (
          currentIndex !== -1 &&
          currentIndex < currentProjectTracks.length - 1
        ) {
          return currentProjectTracks[currentIndex + 1];
        }
      }
    }

    return null;
  }, [
    currentTrack,
    queue,
    currentProjectTracks,
    shuffledProjectTracks,
    isShuffled,
  ]);
```

- [ ] **Step 3: Add the preload trigger effect**

Add this effect after `getNextTrack` is defined (it depends on it):

```ts
  // Derived boolean, not raw progress: previewProgress updates every frame and
  // depending on it directly would re-run this effect ~60x/second.
  const inPreloadWindow = shouldStartPreload({
    currentTime: previewProgress,
    duration,
  });

  useEffect(() => {
    if (!isPlaying || !currentTrack) return;
    // Native looping means `ended` never fires, so there is nothing to preload.
    if (loopMode === "track") return;
    if (!inPreloadWindow) return;

    const next = getNextTrack();
    if (!next) {
      setNextTrackPreload(null);
      preloadKeyRef.current = null;
      return;
    }

    const key = `${currentTrack.id}:${next.id}:${qualityPreference}`;
    if (preloadKeyRef.current === key) return;
    preloadKeyRef.current = key;

    let cancelled = false;

    void (async () => {
      if (!shareTokenRef.current && !isAuthenticated) return;

      try {
        let url: string;

        if (shareTokenRef.current) {
          url = resolveApiUrl(
            `/api/share/${shareTokenRef.current}/stream/${next.id}`,
          );
        } else {
          const signed = await getStreamUrl(next.id, {
            quality: qualityPreference,
            versionId: next.versionId ?? undefined,
          });
          url = resolveApiUrl(signed.url);
        }

        if (cancelled) return;
        setNextTrackPreload({ trackId: next.id, url, signedAt: Date.now() });
      } catch (error) {
        console.error("[AudioPlayer] Failed to preload next track:", error);
        // Re-arm so a later frame can retry.
        if (!cancelled) preloadKeyRef.current = null;
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [
    isPlaying,
    currentTrack,
    loopMode,
    inPreloadWindow,
    qualityPreference,
    isAuthenticated,
    getNextTrack,
  ]);
```

- [ ] **Step 4: Re-mint a stale URL on resume**

Add immediately after the trigger effect:

```ts
  // The user can pause inside the preload window and come back after the
  // signed URL has aged out. Drop it on resume so the trigger re-mints.
  useEffect(() => {
    if (!isPlaying || !nextTrackPreload) return;
    if (isPreloadStale({ signedAt: nextTrackPreload.signedAt, now: Date.now() })) {
      preloadKeyRef.current = null;
      setNextTrackPreload(null);
    }
  }, [isPlaying, nextTrackPreload]);
```

- [ ] **Step 5: Clear the preload when the track changes**

Add after the effect above:

```ts
  useEffect(() => {
    setNextTrackPreload(null);
    preloadKeyRef.current = null;
  }, [currentTrack?.id]);
```

- [ ] **Step 6: Clear the preload on sign-out**

In the `if (!isAuthenticated)` effect, alongside the other resets (`setCurrentTrack(null)`, `setAudioUrl(null)`, ...), add:

```ts
      setNextTrackPreload(null);
      preloadKeyRef.current = null;
```

- [ ] **Step 7: Publish it**

Add to the provider `value={{ ... }}` object:

```ts
        nextTrackPreload,
```

- [ ] **Step 8: Buffer the preload into the standby element**

In `frontend/src/components/MusicPlayer.tsx`, add to the imports:

```ts
import { chooseTransition } from "../lib/gaplessPreload";
```

Add `nextTrackPreload` to the `useAudioPlayer()` destructure.

Add this effect after the `audioRef` repoint effect from Task 3:

```ts
  // Buffer the next track into whichever element is not currently playing.
  useEffect(() => {
    if (!nextTrackPreload) return;
    const standby = getElement(activeKey === "a" ? "b" : "a");
    if (!standby) return;
    if (standby.src === nextTrackPreload.url) return;

    standby.src = nextTrackPreload.url;
    standby.volume = volumePercentage / 100;
    standby.load();
  }, [nextTrackPreload, activeKey, getElement, volumePercentage]);
```

No `error` listener is needed here. A standby that fails to load — a 403 from an
aged-out signed URL, a network error — stays at `readyState` 0, so
`chooseTransition` in Step 9 returns `"load"` and the transition falls back to
the existing path. Handling the error event separately would be a second code
path to the same outcome.

- [ ] **Step 9: Swap instead of loading when the standby is ready**

Replace the body of the `audioUrl` effect (the one starting `if (!audioUrl || !audioRef.current) return;`, around line 462) with:

```ts
  useEffect(() => {
    if (!audioUrl) return;

    const active = getElement(activeKey);
    const standby = getElement(activeKey === "a" ? "b" : "a");
    if (!active) return;
    if (active.src === audioUrl) return;

    const decision = chooseTransition({
      standbySrc: standby?.src || null,
      targetUrl: audioUrl,
      readyState: standby?.readyState ?? 0,
    });

    if (decision === "swap" && standby) {
      // Tear the outgoing element down BEFORE starting the incoming one.
      // A half-finished swap plays two tracks at once, which is worse than a gap.
      active.pause();
      active.removeAttribute("src");
      active.load();

      standby.volume = volumePercentage / 100;
      setActiveKey(activeKey === "a" ? "b" : "a");

      if (isPlaying) {
        standby.play().catch((error) => {
          console.error("Failed to play swapped element:", error);
        });
      }
      return;
    }

    // Fallback: the existing load path, unchanged.
    active.src = audioUrl;
    active.volume = volumePercentage / 100;
    active.load();

    if (isPlaying) {
      const handleLoadedData = () => {
        active.play().catch((error) => {
          console.error("Failed to play:", error);
        });
      };

      active.addEventListener("loadeddata", handleLoadedData, { once: true });

      const handleCanPlay = () => {
        if (active.paused && active.readyState >= 2) {
          active.play().catch((error) => {
            console.error("Failed to play:", error);
          });
        }
      };

      active.addEventListener("canplay", handleCanPlay, { once: true });

      return () => {
        active.removeEventListener("loadeddata", handleLoadedData);
        active.removeEventListener("canplay", handleCanPlay);
      };
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [audioUrl, activeKey, getElement, isPlaying, volumePercentage]);
```

- [ ] **Step 10: Verify**

```bash
cd frontend && bunx tsc --noEmit && bun run test
```

Expected: no TypeScript errors, all tests pass.

Then, in a desktop browser with devtools Network open:

```bash
cd frontend && bun run dev
```

Confirm: no request for the next track until ~20s remain; one request for the next track's stream URL at ~20s remaining; the transition happens with no visible reload; exactly one track is audible at a time.

- [ ] **Step 11: Commit**

```bash
git add frontend/src/contexts/AudioPlayerContext.tsx frontend/src/components/MusicPlayer.tsx
git commit -m "Preload next track at ~20s remaining and swap elements"
```

---

### Task 5: iOS gesture unlock for the standby element

iOS Safari throttles `preload` on an element that has never been touched by a user gesture. Without this, the standby buffers nothing and Task 4 silently degrades to the fallback path on every transition.

**Files:**
- Modify: `frontend/src/components/MusicPlayer.tsx`

**Interfaces:**
- Consumes: `SILENT_AUDIO_DATA_URI` from Task 1; `elARef`, `elBRef` from Task 3.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Import the silent source**

Extend the Task 4 import in `frontend/src/components/MusicPlayer.tsx`:

```ts
import { chooseTransition, SILENT_AUDIO_DATA_URI } from "../lib/gaplessPreload";
```

- [ ] **Step 2: Add the unlock routine**

Add near the other `useCallback` definitions, above `handlePlayPause`:

```ts
  const unlockedRef = useRef(false);

  /**
   * iOS Safari will not buffer an <audio> element that has never played inside
   * a user gesture, which would leave the standby empty at every swap. Play and
   * immediately pause both elements, muted, during the first real gesture.
   *
   * A srcless element rejects play(), so elements without a source get a 10ms
   * silent data URI first.
   */
  const unlockAudioElements = useCallback(() => {
    if (unlockedRef.current) return;
    unlockedRef.current = true;

    for (const element of [elARef.current, elBRef.current]) {
      if (!element) continue;
      const wasMuted = element.muted;
      const hadSource = !!element.getAttribute("src");

      if (!hadSource) {
        element.src = SILENT_AUDIO_DATA_URI;
      }

      element.muted = true;
      element
        .play()
        .then(() => {
          element.pause();
          element.muted = wasMuted;
          if (!hadSource) {
            element.removeAttribute("src");
            element.load();
          }
        })
        .catch(() => {
          element.muted = wasMuted;
          if (!hadSource) {
            element.removeAttribute("src");
            element.load();
          }
        });
    }
  }, []);
```

- [ ] **Step 3: Call it from the play gesture**

Change `handlePlayPause` (around line 188) from:

```ts
  const handlePlayPause = useCallback(() => {
    if (isPlaying) {
      pause();
    } else if (currentTrack) {
      resume();
    } else if (queue.length > 0) {
      playFromQueue();
    }
  }, [isPlaying, pause, resume, currentTrack, queue.length, playFromQueue]);
```

to:

```ts
  const handlePlayPause = useCallback(() => {
    unlockAudioElements();

    if (isPlaying) {
      pause();
    } else if (currentTrack) {
      resume();
    } else if (queue.length > 0) {
      playFromQueue();
    }
  }, [
    isPlaying,
    pause,
    resume,
    currentTrack,
    queue.length,
    playFromQueue,
    unlockAudioElements,
  ]);
```

- [ ] **Step 4: Verify**

```bash
cd frontend && bunx tsc --noEmit && bun run test
```

Expected: no TypeScript errors, all tests pass.

In a desktop browser, confirm playback still starts normally on the first click and no silence is audible at the start of a track.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/components/MusicPlayer.tsx
git commit -m "Unlock both audio elements on first play gesture"
```

---

### Task 6: Device verification checklist

None of the following is observable in a unit test or a desktop browser. This task is the acceptance gate for the feature.

**Files:**
- Create: `docs/superpowers/plans/2026-08-25-gapless-playback-device-checklist.md`

**Interfaces:**
- Consumes: a deployed or dev-served build reachable from an iPhone.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Write the checklist**

Create `docs/superpowers/plans/2026-08-25-gapless-playback-device-checklist.md`:

```markdown
# Gapless Playback — iOS Device Checklist

Device: iPhone, Safari 26 or later. Not simulated — the throttling behavior
under test does not reproduce in a desktop browser or a simulator.

Serve the app on the LAN (`vite.config.ts` already sets `host: true`) and open
it from the phone.

## Gate 1 — the standby actually buffers

- [ ] Start a track longer than 40s from a queue of at least two tracks.
- [ ] With Safari Web Inspector attached, watch the Network tab at ~20s
      remaining. A request for the next track's stream URL must appear.
- [ ] The request must transfer a meaningful number of bytes, not just
      respond to a range probe. If it stalls at a few KB, the gesture unlock
      is not working and the rest of this checklist will fail.

## Gate 2 — the seam

- [ ] Let the track run to its end without touching the device.
- [ ] The next track must begin with no visible reload and no more than a
      brief seam. Anything approaching a second means the swap fell back to
      the load path.
- [ ] Repeat across at least three consecutive transitions.

## Gate 3 — no double audio

- [ ] Press Next rapidly five times.
- [ ] Exactly one track must be audible at any moment. Two overlapping tracks
      is a hard failure — the swap teardown ordering is wrong.

## Gate 4 — background and lock screen

- [ ] Start playback, lock the phone.
- [ ] Playback must continue across a track transition while locked.
- [ ] The lock screen Now Playing must update to the new track's title,
      artist, and artwork after the transition.
- [ ] Lock screen play/pause/next must still control playback after a swap.

## Gate 5 — invalidation

- [ ] Inside the last 20s of a track, reorder the queue so a different track
      is next. The newly-chosen track must play next, not the originally
      preloaded one.
- [ ] Inside the last 20s, toggle shuffle. The transition must respect the
      new order.
- [ ] Pause inside the last 20s, wait six minutes, resume, and let the track
      end. The transition must succeed — this exercises the stale signed-URL
      re-mint.

## Gate 6 — degradation

- [ ] Enable Network Link Conditioner with a slow profile. Transitions must
      still work, falling back to the load path rather than failing or
      producing silence.
```

- [ ] **Step 2: Run the checklist on a real device**

Work through every gate above. Record failures as issues before declaring the feature complete.

- [ ] **Step 3: Commit**

```bash
git add docs/superpowers/plans/2026-08-25-gapless-playback-device-checklist.md
git commit -m "Add iOS device verification checklist for gapless playback"
```
