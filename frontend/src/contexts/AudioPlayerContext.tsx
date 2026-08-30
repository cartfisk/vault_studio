import type React from "react";
import {
  createContext,
  useContext,
  useState,
  useRef,
  useEffect,
  useCallback,
} from "react";
import { getStreamUrl, resolveApiMediaUrl } from "../api/media";
import { resolveApiUrl } from "../api/server";
import { createShuffledPlaylist } from "../lib/optimalShuffle";
import { useAuth } from "./AuthContext";
import { usePreferences } from "./PreferencesContext";
import { getTrack as fetchTrack } from "../api/tracks";
import { getVersions } from "../api/versions";
import { preloadCover } from "../hooks/useProjectCoverImage";
import {
  hasNativeMediaSession,
  NativeMediaSession,
  type NativeMediaActionEvent,
} from "../lib/nativeMediaSession";
import {
  isPreloadStale,
  shouldStartPreload,
} from "../lib/gaplessPreload";
import {
  codecsParam,
  supportedLosslessCodecs,
} from "../lib/playback/codecSupport";
import {
  canAppendNext,
  selectEngine,
  type EngineKind,
} from "../lib/playback/selectEngine";
import type {
  GaplessManifest,
  PlayableTrack,
  PlaybackEngine,
} from "../lib/playback/types";

interface Track {
  id: string;
  title: string;
  artist?: string | null;
  projectName?: string;
  coverUrl?: string | null;
  projectId?: string;
  projectCoverUrl?: string;
  waveform?: string | null;
  lossy_transcoding_status?:
    | "pending"
    | "processing"
    | "completed"
    | "failed"
    | null;
  visibility_status?: "public" | "private" | "unlisted";
  versionId?: number | null;
  isSharedTrack?: boolean;
}

export type LoopMode = "off" | "track" | "project";

export interface NextTrackPreload {
  trackId: string;
  versionId: number | null;
  url: string;
  /** Date.now() at signing time, used to detect expiry before swap. */
  signedAt: number;
  /**
   * Present only when the server offered a lossless rendition for this
   * track. Carried so the preload path can decide whether the next track
   * could join the current timeline rather than force a swap.
   */
  manifest?: GaplessManifest | null;
  /** Which engine this track resolved to, given the manifest above and what
   *  this browser can decode. */
  engine: EngineKind;
}

/**
 * What the context is allowed to talk to. Deliberately narrower than a raw
 * media element: every time value here is TRACK-relative, so no caller can
 * accidentally read a shared-timeline absolute value.
 *
 * `getPlaybackRate` is not part of `PlaybackEngine`; it is supplied by the
 * facade MusicPlayer publishes and only feeds MediaSession position state.
 */
type ContextEngine = Pick<
  PlaybackEngine,
  | "play"
  | "pause"
  | "getTrackTime"
  | "getTrackDuration"
  | "seekToTrackTime"
  | "canAppend"
  | "prepareNext"
  | "teardown"
> & { getPlaybackRate?: () => number };

interface AudioPlayerContextType {
  currentTrack: Track | null;
  isPlaying: boolean;
  duration: number;
  previewProgress: number;
  queue: Track[];
  currentProjectTracks: Track[];
  shuffledProjectTracks: Track[];
  audioUrl: string | null;
  nextTrackPreload: NextTrackPreload | null;
  play: (
    track: Track,
    projectTracks?: Track[],
    autoPlay?: boolean,
    queue?: Track[],
  ) => void;
  pause: () => void;
  resume: () => void;
  stop: () => void;
  nextTrack: () => void;
  previousTrack: () => void;
  playFromQueue: () => void;
  seekTo: (time: number) => void;
  addToQueue: (track: Track) => void;
  addProjectToQueue: (tracks: Track[]) => void;
  removeFromQueue: (index: number) => void;
  clearQueue: () => void;
  reorderQueue: (startIndex: number, endIndex: number) => void;
  setProjectTracks: (tracks: Track[]) => void;
  loopMode: LoopMode;
  toggleLoop: () => void;
  isShuffled: boolean;
  toggleShuffle: () => void;
  onPlayingChange: (playing: boolean) => void;
  onDurationChange: (duration: number) => void;
  onProgressUpdate: (progress: number) => void;
  onEnded: () => void;
  audioPlayerRef: React.RefObject<any>;
  shareToken: string | null;
  sharePassword: string;
  setShareToken: (token: string | null, password?: string) => void;
}

const AudioPlayerContext = createContext<AudioPlayerContextType | undefined>(
  undefined,
);

const QUEUE_STORAGE_KEY = "audioPlayerQueue";
const DEFAULT_AUDIO_QUALITY = "lossy";

export function AudioPlayerProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  const { isAuthenticated } = useAuth();
  const [currentTrack, setCurrentTrack] = useState<Track | null>(null);
  const [isPlaying, setIsPlaying] = useState(false);
  const [duration, setDuration] = useState(0);
  const [previewProgress, setPreviewProgress] = useState(0);
  const [audioUrl, setAudioUrl] = useState<string | null>(null);
  const [nextTrackPreload, setNextTrackPreload] =
    useState<NextTrackPreload | null>(null);
  /**
   * Mirror of the state above. `play()` must read the preload synchronously,
   * before React commits the state change that clears it, so it reads this.
   */
  const nextTrackPreloadRef = useRef<NextTrackPreload | null>(null);
  useEffect(() => {
    nextTrackPreloadRef.current = nextTrackPreload;
  }, [nextTrackPreload]);
  /**
   * `${currentTrackId}:${nextTrackId}:${quality}` for the preload currently in
   * flight or published. Keyed on the tuple rather than a boolean so a queue
   * reorder, shuffle toggle, or quality change re-arms the trigger instead of
   * latching it off.
   */
  const preloadKeyRef = useRef<string | null>(null);
  const [loopMode, setLoopMode] = useState<LoopMode>("off");
  const [isShuffled, setIsShuffled] = useState(false);
  const shareTokenRef = useRef<string | null>(null);
  const [shareToken, setShareTokenValue] = useState<string | null>(null);
  const [sharePassword, setSharePassword] = useState("");
  const [queue, setQueue] = useState<Track[]>(() => {
    try {
      const saved = localStorage.getItem(QUEUE_STORAGE_KEY);
      return saved ? JSON.parse(saved) : [];
    } catch {
      return [];
    }
  });
  const [currentProjectTracks, setCurrentProjectTracks] = useState<Track[]>([]);
  const [shuffledProjectTracks, setShuffledProjectTracks] = useState<Track[]>(
    [],
  );
  const audioPlayerRef = useRef<any>(null);
  /**
   * The engine currently driving playback, or null before MusicPlayer has
   * mounted and published one. Every reach for transport or time goes
   * through here; the context never touches a media element.
   */
  const getEngine = useCallback((): ContextEngine | null => {
    const engine = audioPlayerRef.current;
    return engine && typeof engine.getTrackTime === "function"
      ? (engine as ContextEngine)
      : null;
  }, []);
  /** Which engine the track that is playing right now resolved to. Read by
   *  the preload trigger to decide append-vs-handoff. */
  const currentEngineKindRef = useRef<EngineKind>("elementPair");
  const playRequestIdRef = useRef(0);
  const { preferences } = usePreferences();
  const qualityPreference = preferences?.default_quality || DEFAULT_AUDIO_QUALITY;
  const [shareTokenVersion, setShareTokenVersion] = useState(0);
  const waveformCacheRef = useRef<Record<string, string | null>>({});

  // Dummy read to satisfy TS unused variable detection
  if (shareTokenVersion < 0) {
    console.log(shareTokenVersion);
  }
  const originalQueueRef = useRef<Track[]>([]);
  const isInitialMountRef = useRef(true);
  const lastRestartTimeRef = useRef<number>(0);
  const prevIsShuffledRef = useRef<boolean>(false);

  useEffect(() => {
    try {
      localStorage.setItem(QUEUE_STORAGE_KEY, JSON.stringify(queue));
    } catch (error) {
      console.error("Failed to save queue to localStorage:", error);
    }
  }, [queue]);

  useEffect(() => {
    if (isInitialMountRef.current) {
      isInitialMountRef.current = false;
      prevIsShuffledRef.current = isShuffled;
      return;
    }

    if (prevIsShuffledRef.current === isShuffled) {
      return;
    }
    prevIsShuffledRef.current = isShuffled;

    if (isShuffled) {
      setQueue((currentQueue) => {
        if (currentQueue.length > 0) {
          originalQueueRef.current = [...currentQueue];
          return createShuffledPlaylist(currentQueue);
        }
        return currentQueue;
      });
    } else {
      if (currentTrack && currentProjectTracks.length > 0) {
        const currentIndex = currentProjectTracks.findIndex(
          (t) => t.id === currentTrack.id,
        );
        if (currentIndex !== -1) {
          const remainingTracks = currentProjectTracks.slice(currentIndex + 1);
          setQueue(remainingTracks);
        } else {
          setQueue([]);
        }
      } else {
        setQueue([]);
      }
      originalQueueRef.current = [];
    }
  }, [isShuffled, currentTrack, currentProjectTracks]);



  const getWaveformCacheKey = useCallback((track: Track): string => {
    return `${track.id}:${track.versionId ?? "active"}`;
  }, []);

  const cacheWaveforms = useCallback(
    (tracks: Track[]) => {
      tracks.forEach((track) => {
        if (track.waveform) {
          waveformCacheRef.current[getWaveformCacheKey(track)] = track.waveform;
        }
      });
    },
    [getWaveformCacheKey],
  );

  const ensureTrackWaveform = useCallback(
    async (track: Track): Promise<Track> => {
      if (!track.id) return track;
      const cacheKey = getWaveformCacheKey(track);

      const cachedWaveform = waveformCacheRef.current[cacheKey];
      if (cachedWaveform !== undefined) {
        return cachedWaveform ? { ...track, waveform: cachedWaveform } : track;
      }

      try {
        if (track.versionId) {
          const versions = await getVersions(track.id);
          const selectedVersion = versions.find(
            (version) => version.id === track.versionId,
          );
          const versionWaveform = selectedVersion?.waveform ?? null;

          waveformCacheRef.current[cacheKey] = versionWaveform;
          if (versionWaveform) {
            return { ...track, waveform: versionWaveform };
          }
          // Fall through to full track fetch; do not trust track.waveform - it may be from another version
        } else if (track.waveform) {
          waveformCacheRef.current[cacheKey] = track.waveform;
          return track;
        }

        const fullTrack = await fetchTrack(track.id);
        waveformCacheRef.current[cacheKey] = fullTrack.waveform ?? null;
        if (fullTrack.waveform) {
          return { ...track, waveform: fullTrack.waveform };
        }
      } catch (error) {
        console.error(
          `[AudioPlayer] Failed to load waveform for track ${track.id}:`,
          error,
        );
        waveformCacheRef.current[cacheKey] = null;
      }

      return track;
    },
    [getWaveformCacheKey],
  );

  useEffect(() => {
    if (isShuffled && currentProjectTracks.length > 0) {
      const shuffled = createShuffledPlaylist(currentProjectTracks);
      setShuffledProjectTracks(shuffled);
    } else {
      setShuffledProjectTracks([]);
    }
  }, [isShuffled, currentProjectTracks]);

  const play = useCallback(
    async (
      track: Track,
      projectTracks?: Track[],
      autoPlay: boolean = true,
      queueTracks?: Track[],
    ) => {
      const requestId = ++playRequestIdRef.current;

      if (
        track.lossy_transcoding_status &&
        track.lossy_transcoding_status !== "completed"
      ) {
        return;
      }

      if (projectTracks) {
        setCurrentProjectTracks(projectTracks);
        cacheWaveforms(projectTracks);
        if (isShuffled) {
          const shuffled = createShuffledPlaylist(projectTracks);
          setShuffledProjectTracks(shuffled);
        } else {
          setShuffledProjectTracks([]);
        }
      }

      if (queueTracks) {
        if (isShuffled) {
          originalQueueRef.current = [...queueTracks];
          setQueue(createShuffledPlaylist(queueTracks));
        } else {
          setQueue(queueTracks);
        }
        cacheWaveforms(queueTracks);
      }

      let trackToPlay = track;
      trackToPlay = await ensureTrackWaveform(trackToPlay);

      setCurrentTrack(trackToPlay);

      const quality = qualityPreference;

      if (playRequestIdRef.current !== requestId) {
        return;
      }

      let streamUrl: string;
      let manifest: GaplessManifest | null = null;

      if (shareTokenRef.current) {
        streamUrl = resolveApiUrl(
          `/api/share/${shareTokenRef.current}/stream/${trackToPlay.id}`,
        );
      } else {
        const preload = nextTrackPreloadRef.current;
        const preloadMatches =
          preload !== null &&
          preload.trackId === trackToPlay.id &&
          preload.versionId === (trackToPlay.versionId ?? null) &&
          !isPreloadStale({ signedAt: preload.signedAt, now: Date.now() });

        if (preloadMatches && preload) {
          streamUrl = preload.url;
          // Carry the preload's manifest along. Dropping it here would
          // silently downgrade a track the preload had already resolved as
          // gapless-capable.
          manifest = preload.manifest ?? null;
        } else {
          try {
            const codecs = codecsParam();
            const signed = await getStreamUrl(trackToPlay.id, {
              quality,
              versionId: trackToPlay.versionId ?? undefined,
              // Omitted entirely when null: a present-but-empty `codecs`
              // opts into the server's gapless branch, which a browser that
              // can decode nothing lossless must not do.
              ...(codecs ? { codecs } : {}),
            });
            streamUrl = resolveApiUrl(signed.url);
            manifest = signed.gapless ?? null;
          } catch (error) {
            console.error("[AudioPlayer] Failed to get signed stream URL", error);
            return;
          }
        }
      }

      currentEngineKindRef.current = selectEngine({
        manifest,
        supported: supportedLosslessCodecs(),
      });

      setAudioUrl(streamUrl);
      setIsPlaying(autoPlay);
    },
    [isShuffled, ensureTrackWaveform, cacheWaveforms],
  );

  const pause = useCallback(() => {
    setIsPlaying(false);
    getEngine()?.pause();
  }, [getEngine]);

  const resume = useCallback(() => {
    setIsPlaying(true);
    void getEngine()?.play();
  }, [getEngine]);

  const stop = useCallback(() => {
    setIsPlaying(false);
    setCurrentTrack(null);
    setAudioUrl(null);
    setDuration(0);
    setPreviewProgress(0);
    const engine = getEngine();
    if (engine) {
      engine.pause();
      engine.seekToTrackTime(0);
    }
  }, [getEngine]);

  const playFromQueue = useCallback(() => {
    if (queue.length === 0) return;

    const first = queue[0];
    setQueue((prev) => prev.slice(1));
    play(first);
  }, [queue, play]);

  const nextTrack = useCallback(() => {
    if (!currentTrack) {
      if (queue.length > 0) {
        playFromQueue();
      }
      return;
    }

    if (queue.length > 0) {
      const next = queue[0];
      setQueue((prev) => prev.slice(1));
      play(next);
      return;
    }

    if (currentProjectTracks.length > 0) {
      const tracksToUse = isShuffled
        ? shuffledProjectTracks
        : currentProjectTracks;
      const currentIndex = tracksToUse.findIndex(
        (t) => t.id === currentTrack.id,
      );

      if (currentIndex !== -1 && currentIndex < tracksToUse.length - 1) {
        play(tracksToUse[currentIndex + 1]);
        return;
      }

      pause();

      const remainingTracks = currentProjectTracks.slice(1);
      const tracksToQueue = isShuffled
        ? createShuffledPlaylist(remainingTracks)
        : remainingTracks;
      setQueue(tracksToQueue);

      play(currentProjectTracks[0], undefined, false);
      return;
    }

    pause();
  }, [
    currentTrack,
    queue,
    currentProjectTracks,
    shuffledProjectTracks,
    isShuffled,
    play,
    pause,
    playFromQueue,
  ]);

  const previousTrack = useCallback(() => {
    if (!currentTrack) return;

    const now = Date.now();
    const DOUBLE_TAP_THRESHOLD = 1500;
    const recentlyRestarted =
      now - lastRestartTimeRef.current < DOUBLE_TAP_THRESHOLD;
    // TRACK-relative, never the element's `currentTime`. Under MSE every
    // track after the first sits at a large absolute timeline offset, so a
    // raw read is always > 3 and the back button would silently degrade into
    // "restart the track", never reaching the previous one.
    const engine = getEngine();
    const currentTime = engine?.getTrackTime() ?? 0;

    if (!recentlyRestarted && currentTime > 3) {
      engine?.seekToTrackTime(0);
      lastRestartTimeRef.current = now;
      return;
    }

    if (currentProjectTracks.length > 0) {
      if (isShuffled && shuffledProjectTracks.length > 0) {
        const currentIndex = shuffledProjectTracks.findIndex(
          (t) => t.id === currentTrack.id,
        );
        if (currentIndex > 0) {
          setQueue((prev) => [currentTrack, ...prev]);
          play(shuffledProjectTracks[currentIndex - 1]);
          return;
        }
      } else {
        const currentIndex = currentProjectTracks.findIndex(
          (t) => t.id === currentTrack.id,
        );
        if (currentIndex > 0) {
          setQueue((prev) => [currentTrack, ...prev]);
          play(currentProjectTracks[currentIndex - 1]);
          return;
        }
      }
    }

    if (engine) {
      engine.seekToTrackTime(0);
      lastRestartTimeRef.current = now;
    }
  }, [
    currentTrack,
    currentProjectTracks,
    shuffledProjectTracks,
    isShuffled,
    play,
    getEngine,
  ]);

  const seekTo = useCallback(
    (time: number) => {
      const engine = getEngine();
      if (engine) {
        engine.seekToTrackTime(time);
      } else {
        console.error("[AudioPlayer] Audio ref not available for seeking");
      }
    },
    [getEngine],
  );

  const onEnded = useCallback(() => {
    if (loopMode === "track") {
      return;
    }

    if (loopMode === "project") {
      if (currentProjectTracks.length > 0 && currentTrack) {
        const tracksToUse = isShuffled
          ? shuffledProjectTracks
          : currentProjectTracks;
        const currentIndex = tracksToUse.findIndex(
          (t) => t.id === currentTrack.id,
        );

        if (currentIndex === tracksToUse.length - 1 && queue.length === 0) {
          play(tracksToUse[0]);
          return;
        }
      }
    }

    nextTrack();
  }, [
    loopMode,
    nextTrack,
    currentProjectTracks,
    shuffledProjectTracks,
    currentTrack,
    isShuffled,
    queue.length,
    play,
  ]);

  const onPlayingChange = useCallback((playing: boolean) => {
    setIsPlaying(playing);
  }, []);

  const onDurationChange = useCallback((duration: number) => {
    setDuration(duration);
  }, []);

  const onProgressUpdate = useCallback((progress: number) => {
    setPreviewProgress(progress);
  }, []);

  const addToQueue = useCallback(
    (track: Track) => {
      if (!currentTrack) {
        play(track, undefined, false);
        return;
      }

      setQueue((prev) => [...prev, track]);
      if (isShuffled && originalQueueRef.current.length > 0) {
        originalQueueRef.current = [...originalQueueRef.current, track];
      }
    },
    [isShuffled, currentTrack, play],
  );

  const removeFromQueue = useCallback(
    (index: number) => {
      setQueue((prev) => {
        const trackToRemove = prev[index];

        if (
          isShuffled &&
          originalQueueRef.current.length > 0 &&
          trackToRemove
        ) {
          const originalIndex = originalQueueRef.current.findIndex(
            (t) => t.id === trackToRemove.id,
          );
          if (originalIndex !== -1) {
            originalQueueRef.current = originalQueueRef.current.filter(
              (_, i) => i !== originalIndex,
            );
          }
        }

        return prev.filter((_, i) => i !== index);
      });
    },
    [isShuffled],
  );

  const clearQueue = useCallback(() => {
    setQueue([]);
    originalQueueRef.current = [];
  }, []);

  const addProjectToQueue = useCallback(
    (tracks: Track[]) => {
      if (tracks.length === 0) return;

      if (!currentTrack) {
        const [firstTrack, ...rest] = tracks;
        play(firstTrack, undefined, false, rest);
        return;
      }

      cacheWaveforms(tracks);
      if (isShuffled) {
        originalQueueRef.current = [...originalQueueRef.current, ...tracks];
        setQueue((prev) => [...prev, ...createShuffledPlaylist(tracks)]);
      } else {
        setQueue((prev) => [...prev, ...tracks]);
      }
    },
    [isShuffled, currentTrack, play, cacheWaveforms],
  );

  const reorderQueue = useCallback((startIndex: number, endIndex: number) => {
    setQueue((prev) => {
      const result = Array.from(prev);
      const [removed] = result.splice(startIndex, 1);
      result.splice(endIndex, 0, removed);
      return result;
    });
  }, []);

  const toggleLoop = useCallback(() => {
    setLoopMode((prev) => {
      if (prev === "off") return "track";
      if (prev === "track") return "project";
      return "off";
    });
  }, []);

  const toggleShuffle = useCallback(() => {
    setIsShuffled((prev: boolean) => !prev);
  }, []);

  const setProjectTracks = useCallback(
    (tracks: Track[]) => {
      setCurrentProjectTracks(tracks);
      cacheWaveforms(tracks);
    },
    [cacheWaveforms],
  );

  const setShareToken = useCallback((token: string | null, password = "") => {
    shareTokenRef.current = token;
    setShareTokenValue(token);
    setSharePassword(password);
    setShareTokenVersion((version) => version + 1);
  }, []);

  useEffect(() => {
    if (hasNativeMediaSession) {
      if (!currentTrack) return;

      const artist =
        (typeof currentTrack.artist === "string" &&
        currentTrack.artist.trim().length > 0
          ? currentTrack.artist
          : "Unknown Artist");
      const artworkUrl = resolveApiMediaUrl(currentTrack.projectCoverUrl);

      void NativeMediaSession.setMetadata({
        title: currentTrack.title || "Unknown Track",
        artist,
        album: currentTrack.projectName ?? "{ vault.studio }",
        ...(artworkUrl ? { artworkUrl } : {}),
      }).catch((error) => {
        console.error("[Native Media Session] Failed to set metadata:", error);
      });
      return;
    }

    if (!("mediaSession" in navigator) || !currentTrack) {
      if ("mediaSession" in navigator) {
        navigator.mediaSession.metadata = null;
      }
      return;
    }

    const artwork = currentTrack.coverUrl
      ? [
          { src: currentTrack.coverUrl, sizes: "512x512", type: "image/jpeg" },
          { src: currentTrack.coverUrl, sizes: "256x256", type: "image/jpeg" },
        ]
      : undefined;

    const metadata = {
      title: currentTrack.title || "Unknown Track",
      artist:
        (typeof currentTrack.artist === "string" &&
        currentTrack.artist.trim().length > 0
          ? currentTrack.artist
          : "Unknown Artist"),
      album: currentTrack.projectName ?? "{ vault.studio }",
      ...(artwork && { artwork }),
    };

    navigator.mediaSession.metadata = new MediaMetadata(metadata);
  }, [currentTrack]);

  useEffect(() => {
    if (queue.length === 0) return;

    const nextTrack = queue[0];
    if (!nextTrack?.projectId || !nextTrack?.projectCoverUrl) return;

    preloadCover(
      { public_id: nextTrack.projectId, cover_url: nextTrack.projectCoverUrl },
      "small",
    );
  }, [queue, currentTrack]);

  useEffect(() => {
    if (hasNativeMediaSession) {
      void NativeMediaSession.setPlaybackState({
        state: currentTrack ? (isPlaying ? "playing" : "paused") : "none",
      }).catch((error) => {
        console.error(
          "[Native Media Session] Failed to set playback state:",
          error,
        );
      });
      return;
    }

    if (!("mediaSession" in navigator)) return;
    navigator.mediaSession.playbackState = currentTrack
      ? isPlaying
        ? "playing"
        : "paused"
      : "none";
  }, [currentTrack, isPlaying]);

  useEffect(() => {
    if (hasNativeMediaSession) {
      let listener: { remove: () => Promise<void> } | undefined;
      let disposed = false;

      const handleAction = (event: NativeMediaActionEvent) => {
        switch (event.action) {
          case "play":
            resume();
            break;
          case "pause":
            pause();
            break;
          case "previousTrack":
            previousTrack();
            break;
          case "nextTrack":
            nextTrack();
            break;
          case "seekTo":
            if (event.seekTime !== undefined) seekTo(event.seekTime);
            break;
          case "stop":
            stop();
            break;
        }
      };

      void NativeMediaSession.addListener("action", handleAction).then(
        (handle) => {
          if (disposed) {
            void handle.remove();
          } else {
            listener = handle;
          }
        },
        (error) => {
          console.error(
            "[Native Media Session] Failed to register controls:",
            error,
          );
        },
      );

      return () => {
        disposed = true;
        if (listener) void listener.remove();
      };
    }

    if (!("mediaSession" in navigator)) return;

    const handlers = {
      play: () => resume(),
      pause: () => pause(),
      previoustrack: () => previousTrack(),
      nexttrack: () => nextTrack(),
      seekto: (details: MediaSessionActionDetails) => {
        if (details.seekTime !== undefined) {
          seekTo(details.seekTime);
        }
      },
    };

    Object.entries(handlers).forEach(([action, handler]) => {
      try {
        navigator.mediaSession.setActionHandler(action as any, handler);
      } catch (error) {
        console.error(`[Media Session] ${action} not supported`);
      }
    });

    return () => {
      if ("mediaSession" in navigator) {
        navigator.mediaSession.playbackState = "none";
      }
    };
  }, [resume, pause, previousTrack, nextTrack, seekTo, stop]);

  useEffect(() => {
    if (!currentTrack) return;

    if (hasNativeMediaSession) {
      const updateNativePosition = () => {
        // Track-relative, not element-absolute: under MSE the element's
        // `duration` is the shared timeline's and GROWS as tracks are
        // appended, which would show a lock-screen total that keeps moving.
        const engine = getEngine();
        if (!engine) return;
        const trackDuration = engine.getTrackDuration();
        if (Number.isNaN(trackDuration) || trackDuration <= 0) {
          return;
        }

        void NativeMediaSession.setPositionState({
          duration: trackDuration,
          playbackRate: engine.getPlaybackRate?.() ?? 1,
          position: engine.getTrackTime(),
        }).catch((error) => {
          console.error(
            "[Native Media Session] Failed to set position:",
            error,
          );
        });
      };

      updateNativePosition();
      const interval = setInterval(updateNativePosition, 1000);
      return () => clearInterval(interval);
    }

    if (!("mediaSession" in navigator)) return;

    const updatePosition = () => {
      const engine = getEngine();
      if (!engine) return;

      const trackDuration = engine.getTrackDuration();
      if (!Number.isNaN(trackDuration) && trackDuration > 0) {
        try {
          navigator.mediaSession.setPositionState({
            duration: trackDuration,
            playbackRate: engine.getPlaybackRate?.() ?? 1,
            position: engine.getTrackTime(),
          });
        } catch (error) {
          console.error("[Media Session] Position state not supported:", error);
        }
      }
    };

    updatePosition();
    const interval = setInterval(updatePosition, 1000);

    return () => {
      clearInterval(interval);
    };
  }, [currentTrack, duration, getEngine]);

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

    // Warm the waveform cache too. `play()` awaits `ensureTrackWaveform` before
    // it can call `setAudioUrl`, so an unwarmed cache puts a full network round
    // trip inside the `ended` -> swap path — exactly the dead air this feature
    // exists to remove. Deliberately not awaited: it must not gate the URL mint.
    void ensureTrackWaveform(next);

    let cancelled = false;

    void (async () => {
      if (!shareTokenRef.current && !isAuthenticated) return;

      try {
        let url: string;
        let manifest: GaplessManifest | null = null;

        if (shareTokenRef.current) {
          url = resolveApiUrl(
            `/api/share/${shareTokenRef.current}/stream/${next.id}`,
          );
        } else {
          const codecs = codecsParam();
          const signed = await getStreamUrl(next.id, {
            quality: qualityPreference,
            versionId: next.versionId ?? undefined,
            // Omitted entirely when null — see the note in `play()`.
            ...(codecs ? { codecs } : {}),
          });
          url = resolveApiUrl(signed.url);
          manifest = signed.gapless ?? null;
        }

        if (cancelled) return;

        const nextEngineKind = selectEngine({
          manifest,
          supported: supportedLosslessCodecs(),
        });
        const playable: PlayableTrack = {
          trackId: next.id,
          versionId: next.versionId ?? null,
          url,
          manifest,
        };

        const engine = getEngine();
        // Append only when BOTH sides agree: the pure selection says this is
        // an MSE-to-MSE transition, and the live engine confirms it can take
        // the track. Anything else is a handoff — the outgoing engine is torn
        // down and the incoming one loads, which costs the seam the lossy
        // tier has always had. A seam is today's behaviour; guessing wrong
        // here is silence.
        const append =
          canAppendNext(currentEngineKindRef.current, nextEngineKind) &&
          (engine?.canAppend(playable) ?? false);

        if (append) {
          await engine?.prepareNext(playable);
        }

        // Published either way. On the append path it is what lets `play()`
        // reuse the already-minted URL instead of re-signing; on the handoff
        // path it is also what drives the element-pair standby buffering.
        setNextTrackPreload({
          trackId: next.id,
          versionId: next.versionId ?? null,
          url,
          signedAt: Date.now(),
          manifest,
          engine: nextEngineKind,
        });
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
    ensureTrackWaveform,
    getEngine,
  ]);

  // The user can pause inside the preload window and come back after the
  // signed URL has aged out. Drop it on resume so the trigger re-mints.
  useEffect(() => {
    if (!isPlaying || !nextTrackPreload) return;
    if (
      isPreloadStale({ signedAt: nextTrackPreload.signedAt, now: Date.now() })
    ) {
      preloadKeyRef.current = null;
      setNextTrackPreload(null);
    }
  }, [isPlaying, nextTrackPreload]);

  useEffect(() => {
    setNextTrackPreload(null);
    preloadKeyRef.current = null;
  }, [currentTrack?.id]);

  useEffect(() => {
    if (!isAuthenticated) {
      setIsPlaying(false);
      // Tear down BOTH elements. The standby can be holding a fully buffered
      // signed stream belonging to the account that just signed out, and it
      // would otherwise stay resident until some later preload overwrote it.
      getEngine()?.teardown();

      setCurrentTrack(null);
      setAudioUrl(null);
      setNextTrackPreload(null);
      preloadKeyRef.current = null;
      setDuration(0);
      setPreviewProgress(0);
      setCurrentProjectTracks([]);
      setShuffledProjectTracks([]);
      clearQueue();

      if ("mediaSession" in navigator) {
        navigator.mediaSession.metadata = null;
        navigator.mediaSession.playbackState = "none";
      }

      if (hasNativeMediaSession) {
        void NativeMediaSession.setPlaybackState({ state: "none" });
      }

      setShareTokenVersion((version) => version + 1);
    }
  }, [isAuthenticated, clearQueue, getEngine]);

  return (
    <AudioPlayerContext.Provider
      value={{
        currentTrack,
        isPlaying,
        duration,
        previewProgress,
        queue,
        currentProjectTracks,
        shuffledProjectTracks,
        audioUrl,
        nextTrackPreload,
        play,
        pause,
        resume,
        stop,
        nextTrack,
        previousTrack,
        playFromQueue,
        seekTo,
        addToQueue,
        addProjectToQueue,
        removeFromQueue,
        clearQueue,
        reorderQueue,
        setProjectTracks,
        loopMode,
        toggleLoop,
        isShuffled,
        toggleShuffle,
        onPlayingChange,
        onDurationChange,
        onProgressUpdate,
        onEnded,
        audioPlayerRef,
        shareToken,
        sharePassword,
        setShareToken,
      }}
    >
      {children}
    </AudioPlayerContext.Provider>
  );
}

export function useAudioPlayer() {
  const context = useContext(AudioPlayerContext);
  if (!context) {
    throw new Error("useAudioPlayer must be used within AudioPlayerProvider");
  }
  return context;
}
