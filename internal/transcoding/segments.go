package transcoding

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// FragmentDurationMicros is the fragment duration requested of ffmpeg.
// The layout actually produced is measured, never assumed to match this.
const FragmentDurationMicros = 10000000

// fragMovFlags is the invocation proven on hardware by the MSE spike.
const fragMovFlags = "+frag_keyframe+empty_moov+default_base_moof"

// SegmentCodecs are the lossless codecs served for gapless playback, one per
// browser engine: ALAC for Safari, FLAC for Chrome and Firefox.
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

// BuildSegmentSet encodes or remuxes sourcePath into a fragmented MP4 at
// outputPath, then measures what it produced.
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
		"-y", outputPath,
	)
	out, cmdErr := cmd.CombinedOutput()

	// ffmpeg may have written outputPath (whole or partial) by this point.
	// If this function ends up returning an error for any reason below,
	// remove it rather than leave a plausible-looking file on disk for a
	// version that has no valid segment set. Ignore the removal's own
	// error; it must never mask the real failure.
	defer func() {
		if err != nil {
			os.Remove(outputPath)
		}
	}()

	if cmdErr != nil {
		return nil, fmt.Errorf("ffmpeg %s failed: %w: %s", targetCodec, cmdErr, out)
	}

	probe, err := probeSegmentFile(outputPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("open produced file: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat produced file: %w", err)
	}

	layout, err := ScanFragments(f, stat.Size())
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
func BuildAllSegmentSets(sourcePath, versionDir, sourceCodec string) (sets []*SegmentSet, err error) {
	built := make([]*SegmentSet, 0, len(SegmentCodecs))

	// If any codec fails, or the cross-check below disagrees, remove every
	// file successfully built so far rather than leave a partial set (e.g.
	// a valid gapless-alac.mp4 with no matching FLAC) on disk.
	defer func() {
		if err != nil {
			for _, s := range built {
				os.Remove(s.FilePath)
			}
		}
	}()

	for _, codec := range SegmentCodecs {
		out := filepath.Join(versionDir, "gapless-"+codec+".mp4")
		set, buildErr := BuildSegmentSet(sourcePath, out, codec, sourceCodec)
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

	return built, nil
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
