package transcoding

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FragmentDurationMicros is the fragment duration requested of ffmpeg.
// The layout actually produced is measured, never assumed to match this.
const FragmentDurationMicros = 10000000

// fragMovFlags is the invocation proven on hardware by the MSE spike.
const fragMovFlags = "+frag_keyframe+empty_moov+default_base_moof"

// staleStagingAge is how old an orphaned staging or preflight-probe file
// must be before sweepStaleFiles will remove it. A generate-segments run
// stopped mid-build (a SIGINT, the expected way to stop one) leaves its
// unique os.CreateTemp staging file behind forever otherwise -- nothing
// else in the codebase globs, sweeps, or reconciles these at startup. An
// hour is conservative: no single codec's build is expected to run
// anywhere near that long. This floor must stay well above any real build
// duration, or a sweep could delete a concurrent builder's still-live
// staging file, which would defeat the collision-freedom os.CreateTemp
// exists to provide.
const staleStagingAge = time.Hour

// SegmentCodecs are the lossless codecs served for gapless playback, one per
// browser engine: ALAC for Safari, FLAC for Chrome and Firefox.
//
// internal/db/queries/segments.sql's ListLosslessVersionsMissingSegments
// hardcodes len(SegmentCodecs) as 2 -- update that literal if this changes.
var SegmentCodecs = []string{"alac", "flac"}

// SegmentSet is one produced fragmented MP4 and its measured properties.
type SegmentSet struct {
	Codec       string
	FilePath    string
	FileSize    int64
	SampleRate  int
	Channels    int
	SampleCount int64
	Layout      FragmentLayout
}

// BuildSegmentSet encodes or remuxes sourcePath into a fragmented MP4,
// measures what it produced, and only then places it at outputPath.
//
// ffmpeg writes and probes/scans happen against outputPath+".tmp", never
// outputPath itself, and the produced file is renamed onto outputPath only
// once the build is fully validated. No production caller passes a final
// gapless-<codec>.mp4 path here any more -- BuildAllSegmentSets always
// hands this a unique staging path from os.CreateTemp -- but the mechanism
// stays load-bearing rather than vestigial: os.CreateTemp creates
// outputPath as an empty placeholder to reserve the name, and this
// function's rename is what overwrites that placeholder with the real,
// validated file. Whatever currently has outputPath open (a concurrent
// build racing to reserve the same name is not expected, but
// TestBuildSegmentSetIsAtomicToConcurrentReaders holds this guarantee
// directly) never observes a half-written file, because rename is atomic
// within a filesystem.
//
// When sourceCodec already matches targetCodec the audio is stream-copied.
// That is a whole-file remux with no -ss/-t, so it keeps every frame; it is
// not the packet-boundary cutting that corrupts sample-exact work.
func BuildSegmentSet(sourcePath, outputPath, targetCodec, sourceCodec string) (set *SegmentSet, err error) {
	if targetCodec != "alac" && targetCodec != "flac" {
		return nil, fmt.Errorf("unsupported segment codec %q", targetCodec)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	tmpPath := outputPath + ".tmp"

	codecArg := targetCodec
	if sourceCodec == targetCodec {
		codecArg = "copy"
	}

	cmd := exec.Command("ffmpeg", "-v", "error",
		"-i", sourcePath,
		"-vn",
		"-c:a", codecArg,
		"-frag_duration", strconv.Itoa(FragmentDurationMicros),
		"-movflags", fragMovFlags,
		"-f", "mp4",
		"-y", tmpPath,
	)
	out, cmdErr := cmd.CombinedOutput()

	// ffmpeg may have written tmpPath (whole or partial) by this point.
	// If this function ends up returning an error for any reason below,
	// remove it rather than leave it on disk. Ignore the removal's own
	// error; it must never mask the real failure. outputPath is never
	// touched until the rename at the very end, so there is nothing to
	// clean up there.
	defer func() {
		if err != nil {
			os.Remove(tmpPath)
		}
	}()

	if cmdErr != nil {
		return nil, fmt.Errorf("ffmpeg %s failed: %w: %s", targetCodec, cmdErr, out)
	}

	probe, err := probeSegmentFile(tmpPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open produced file: %w", err)
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat produced file: %w", err)
	}

	layout, err := ScanFragments(f, stat.Size())
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("scan %s fragments: %w", targetCodec, err)
	}

	// A track longer than one fragment duration that produced a single
	// fragment was not fragmented. Appending it whole is the case already
	// measured to exhaust the SourceBuffer quota, so refuse it here rather
	// than ship an unplayable set. If this fires, drop +frag_keyframe from
	// fragMovFlags and re-measure — do not relax this check.
	contentMicros := probe.SampleCount * 1000000 / int64(probe.SampleRate)
	if contentMicros > FragmentDurationMicros && len(layout.Fragments) < 2 {
		return nil, fmt.Errorf(
			"%s output is %dus but produced 1 fragment; not fragmented",
			targetCodec, contentMicros,
		)
	}

	if err := os.Rename(tmpPath, outputPath); err != nil {
		return nil, fmt.Errorf("rename %s into place: %w", targetCodec, err)
	}

	return &SegmentSet{
		Codec:       targetCodec,
		FilePath:    outputPath,
		FileSize:    stat.Size(),
		SampleRate:  probe.SampleRate,
		Channels:    probe.Channels,
		SampleCount: probe.SampleCount,
		Layout:      layout,
	}, nil
}

// BuildAllSegmentSets produces one set per codec in SegmentCodecs and refuses
// to return any of them unless they agree on sample count.
//
// The cross-check is why both codecs are built together. A disagreement means
// one encode dropped or padded audio, and placing tracks at running offsets of
// a wrong length is exactly the failure this feature exists to avoid.
//
// Every codec is built into a unique staging file in versionDir, never at
// its final gapless-<codec>.mp4 path. Nothing touches a final path until
// every codec has built AND the cross-check above has passed. That is the
// property that makes the whole set atomic on disk: a persistently-failing
// FLAC build must not disturb an already-completed ALAC file (or vice
// versa) by renaming a rebuilt-but-doomed file over it.
func BuildAllSegmentSets(sourcePath, versionDir, sourceCodec string) (sets []*SegmentSet, err error) {
	stagingPaths := make([]string, 0, len(SegmentCodecs))
	built := make([]*SegmentSet, 0, len(SegmentCodecs))

	// If any codec fails, or the cross-check below disagrees, remove every
	// staging file created so far. Final paths are never touched before
	// this point, so there is nothing to restore there.
	defer func() {
		if err != nil {
			for _, p := range stagingPaths {
				os.Remove(p)
			}
		}
	}()

	// A unique os.CreateTemp name, unlike the old fixed ".tmp" name it
	// replaced, is never reclaimed by anything else in the codebase: there
	// is no sweep, no glob, no startup reconciliation. An operator
	// Ctrl-C'ing a multi-hour run (the expected way to stop one) kills the
	// process before the deferred cleanup above ever runs, so this sweeps
	// any staging or preflight-probe file old enough to be orphaned before
	// creating new ones. The age floor is not optional: an unconditional
	// sweep would delete a concurrent builder's live staging file,
	// destroying the collision-freedom os.CreateTemp exists to provide.
	if serr := sweepStaleFiles(filepath.Join(versionDir, ".gapless-preflight-*"), staleStagingAge); serr != nil {
		return nil, fmt.Errorf("sweep stale preflight probes: %w", serr)
	}

	for _, codec := range SegmentCodecs {
		if serr := sweepStaleFiles(filepath.Join(versionDir, "gapless-"+codec+"-*.mp4*"), staleStagingAge); serr != nil {
			return nil, fmt.Errorf("sweep stale %s staging files: %w", codec, serr)
		}

		// os.CreateTemp both allocates a unique name and creates the file,
		// so two concurrent builders (or a concurrent backfill run) can
		// never collide on the same staging path. BuildSegmentSet writes
		// through its own outputPath+".tmp" and renames onto outputPath
		// only once ffmpeg has produced a valid, fragmented file -- that
		// rename overwrites this placeholder.
		stagingFile, cerr := os.CreateTemp(versionDir, "gapless-"+codec+"-*.mp4")
		if cerr != nil {
			return nil, fmt.Errorf("create staging file for %s: %w", codec, cerr)
		}
		stagingPath := stagingFile.Name()
		stagingFile.Close()
		stagingPaths = append(stagingPaths, stagingPath)

		set, buildErr := BuildSegmentSet(sourcePath, stagingPath, codec, sourceCodec)
		if buildErr != nil {
			return nil, buildErr
		}
		built = append(built, set)
	}

	for _, set := range built[1:] {
		if set.SampleCount != built[0].SampleCount {
			return nil, fmt.Errorf(
				"sample count disagreement: %s=%d, %s=%d",
				built[0].Codec, built[0].SampleCount, set.Codec, set.SampleCount,
			)
		}
	}

	// Every codec built and the cross-check passed: commit each staged
	// file onto its final path and repoint FilePath there. FilePath is
	// what gets written to the database and later served, so a set left
	// pointing at its staging path would look complete while actually
	// pointing nowhere useful.
	//
	// Every destination is checked first: a cross-file atomic rename does
	// not exist on POSIX, so the next best thing is to refuse before the
	// first rename rather than discover the problem halfway through and
	// leave a half-committed set (e.g. a freshly-renamed ALAC file next to
	// a FLAC set that never landed).
	finalPaths := make([]string, len(built))
	for i, set := range built {
		finalPaths[i] = filepath.Join(versionDir, "gapless-"+set.Codec+".mp4")
	}
	if perr := preflightCommitDestinations(finalPaths); perr != nil {
		err = fmt.Errorf("preflight commit destinations: %w", perr)
		return nil, err
	}

	for i, set := range built {
		// Preflight already confirmed this destination is renameable, so a
		// failure here is a genuine race (something changed the
		// destination in the microseconds since preflight ran) or an
		// underlying filesystem error, not the predictable case preflight
		// exists to catch. Deliberately not unwinding it: doing so would
		// mean saving the bytes a rename is about to overwrite before
		// performing it, which is materially more machinery than this
		// earns. The resulting state -- some codecs committed, one
		// rename failed -- can leave a completed row whose file was
		// already swapped for a rebuild whose sibling never landed. That
		// divergence is real, not fictional, but it is benign as long as
		// two ffmpeg builds of the same source stay byte-identical (which
		// they are today, verified by MD5): the stale byte ranges a
		// concurrent reader already has still describe the new file
		// correctly. An ffmpeg version upgrade that changes its output
		// determinism would need this revisited. The caller marks the set
		// failed and a rerun repairs it, same as any other build failure.
		if rerr := os.Rename(set.FilePath, finalPaths[i]); rerr != nil {
			err = fmt.Errorf("commit %s into place: %w", set.Codec, rerr)
			return nil, err
		}
		set.FilePath = finalPaths[i]
	}

	return built, nil
}

// preflightCommitDestinations checks every path in finalPaths for the
// conditions that would make os.Rename onto it fail predictably: the
// destination already existing as a directory, or its parent directory
// refusing new files. It creates and immediately removes a throwaway probe
// file in each destination's parent directory, which is the only reliable,
// portable way to answer "can I write here" -- permission bits alone don't
// account for ACLs, quotas, or read-only mounts. Returns the first problem
// found without touching any of the actual destinations.
func preflightCommitDestinations(finalPaths []string) error {
	for _, finalPath := range finalPaths {
		if info, err := os.Lstat(finalPath); err == nil {
			if info.IsDir() {
				return fmt.Errorf("destination %s exists and is a directory", finalPath)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat destination %s: %w", finalPath, err)
		}

		probe, perr := os.CreateTemp(filepath.Dir(finalPath), ".gapless-preflight-*")
		if perr != nil {
			return fmt.Errorf("destination directory for %s is not writable: %w", finalPath, perr)
		}
		probePath := probe.Name()
		probe.Close()
		os.Remove(probePath)
	}
	return nil
}

// sweepStaleFiles removes every file matching pattern whose modification
// time is older than maxAge. pattern is a filepath.Glob pattern, not a
// directory -- callers scope it to exactly the staging or preflight-probe
// names they own so this can never touch anything else, in particular a
// final gapless-<codec>.mp4 path (verified directly by
// TestSweepStaleStagingFilesNeverMatchesAFinalPath: "gapless-alac.mp4" has
// no hyphen after the codec, so "gapless-alac-*.mp4*" does not match it).
// A glob or stat error is reported; a file that vanished between the glob
// and the stat (removed by a concurrent sweep or its own owner finishing)
// is silently skipped rather than treated as a failure.
func sweepStaleFiles(pattern string, maxAge time.Duration) error {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob %s: %w", pattern, err)
	}

	cutoff := time.Now().Add(-maxAge)
	for _, match := range matches {
		info, statErr := os.Stat(match)
		if statErr != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(match)
		}
	}
	return nil
}

type segmentProbe struct {
	SampleRate  int
	Channels    int
	SampleCount int64
}

// probeSegmentFile reads the true sample count from a produced file.
//
// duration_ts is in time_base units, so the count is scaled by sampleCountFor
// rather than read directly. For these files the time base is normally
// 1/sampleRate and the scaling is a no-op, but relying on that would be an
// assumption about the muxer — sampleCountFor verifies it instead of
// assuming it.
func probeSegmentFile(path string) (segmentProbe, error) {
	var p segmentProbe

	cmd := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=duration_ts,time_base,sample_rate,channels",
		"-of", "json", path,
	)
	out, err := cmd.Output()
	if err != nil {
		return p, fmt.Errorf("ffprobe %s: %w", path, err)
	}

	var parsed struct {
		Streams []struct {
			DurationTS int64  `json:"duration_ts"`
			TimeBase   string `json:"time_base"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return p, fmt.Errorf("parse ffprobe output for %s: %w", path, err)
	}
	if len(parsed.Streams) == 0 {
		return p, fmt.Errorf("no audio stream in %s", path)
	}
	s := parsed.Streams[0]

	rate, err := strconv.Atoi(s.SampleRate)
	if err != nil || rate <= 0 {
		return p, fmt.Errorf("unusable sample_rate %q in %s", s.SampleRate, path)
	}

	num, den, err := parseTimeBase(s.TimeBase)
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}

	count, err := sampleCountFor(s.DurationTS, num, den, int64(rate))
	if err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}

	p.SampleRate = rate
	p.Channels = s.Channels
	p.SampleCount = count

	return p, nil
}

// sampleCountFor scales a stream's duration_ts (in time_base units) to a
// sample count: durationTS * num / den is the duration in seconds, times
// rate samples per second. It is exact only when den evenly divides
// durationTS*num*rate; rather than silently truncate a wrong-but-plausible
// count, it errors loudly, because a wrong sample count here would place
// every later track in the play queue at a wrong offset.
func sampleCountFor(durationTS, num, den, rate int64) (int64, error) {
	total := durationTS * num * rate
	if total%den != 0 {
		return 0, fmt.Errorf(
			"sample count not exact: duration_ts=%d * num=%d * rate=%d = %d, not divisible by den=%d",
			durationTS, num, rate, total, den,
		)
	}

	count := total / den
	if count <= 0 {
		return 0, fmt.Errorf(
			"computed non-positive sample count %d (duration_ts=%d, num=%d, den=%d, rate=%d)",
			count, durationTS, num, den, rate,
		)
	}

	return count, nil
}

func parseTimeBase(tb string) (num, den int64, err error) {
	parts := strings.SplitN(tb, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unparseable time_base %q", tb)
	}
	num, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable time_base numerator in %q", tb)
	}
	den, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || den == 0 {
		return 0, 0, fmt.Errorf("unparseable time_base denominator in %q", tb)
	}
	return num, den, nil
}
