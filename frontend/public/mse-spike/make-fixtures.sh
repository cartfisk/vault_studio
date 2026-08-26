#!/usr/bin/env bash
# Regenerates the MSE spike fixtures. Output is gitignored.
#
# One 20s 440Hz tone, split at exactly 10.000s (441000 samples @ 44.1kHz).
# The split lands mid-cycle, so a bad join clicks audibly.
set -euo pipefail

cd "$(dirname "$0")"
out="fixtures"
mkdir -p "$out"
rm -f "$out"/*.wav "$out"/*.mp4

frag="+frag_keyframe+empty_moov+default_base_moof"

# Continuous source, then two exact halves.
ffmpeg -v error -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=20" \
  -ac 2 -c:a pcm_s16le "$out/source.wav"
# Re-encode rather than -c copy. WAV stream copy cuts at packet boundaries
# (4096 samples), which made h1 run 1368 samples long and overlap h2 by 31ms of
# duplicated audio. Re-encoding cuts sample-exactly.
ffmpeg -v error -i "$out/source.wav" -t 10 -c:a pcm_s16le "$out/h1.wav"
ffmpeg -v error -ss 10 -i "$out/source.wav" -c:a pcm_s16le "$out/h2.wav"

# Round two also builds "-elst" variants: same encode, but WITHOUT +empty_moov.
# empty_moov strips the edit list that carries encoder priming. A fragmented file
# with a real moov is still a valid MSE init segment, so this tests whether Safari
# honours the edit list where appendWindow clipping failed.
frag_elst="+frag_keyframe+default_base_moof"

for half in h1 h2; do
  ffmpeg -v error -i "$out/$half.wav" -c:a aac -b:a 256k \
    -movflags "$frag" -f mp4 "$out/$half-aac.mp4"
  ffmpeg -v error -i "$out/$half.wav" -c:a alac \
    -movflags "$frag" -f mp4 "$out/$half-alac.mp4"
  ffmpeg -v error -i "$out/$half.wav" -c:a aac -b:a 256k \
    -movflags "$frag_elst" -f mp4 "$out/$half-aac-elst.mp4"
  ffmpeg -v error -i "$out/$half.wav" -c:a alac \
    -movflags "$frag_elst" -f mp4 "$out/$half-alac-elst.mp4"
done

# A pure tone compresses to almost nothing in ALAC, which would not exercise
# the memory question at all. Noise is incompressible, so this approximates a
# real lossless track's buffer footprint.
ffmpeg -v error -f lavfi -i "anoisesrc=r=44100:d=240:c=pink" -ac 2 \
  -c:a alac -movflags "$frag" -f mp4 "$out/big-alac.mp4"

# The container duration overshoots the true content: AAC by its priming and
# padding, ALAC by padding its last frame out to 4096 samples. The harness
# needs the true count to trim against, and it is not in the file.
{
  echo '{'
  echo '  "sampleRate": 44100,'
  echo '  "trueSamplesPerHalf": 441000,'
  echo '  "trueDurationPerHalf": 10.0,'
  echo '  "encoded": {'
  first=1
  for f in h1-aac h2-aac h1-alac h2-alac \
           h1-aac-elst h2-aac-elst h1-alac-elst h2-alac-elst; do
    dur=$(ffprobe -v error -show_entries stream=duration \
      -of default=nw=1:nk=1 "$out/$f.mp4")
    # Overshoot beyond the true 441000 samples. For the -elst variants this is
    # the encoder priming the overlap mode needs: the browser is told to place
    # the next segment this much earlier, so its priming lands on the previous
    # segment's tail instead of being clipped at a frame boundary.
    over=$(awk -v d="$dur" 'BEGIN { printf "%d", (d * 44100) - 441000 }')
    [ $first -eq 0 ] && echo ','
    first=0
    printf '    "%s.mp4": { "containerDuration": %s, "overshootSamples": %s }' \
      "$f" "$dur" "$over"
  done
  echo
  echo '  }'
  echo '}'
} > "$out/fixtures.json"

rm -f "$out"/*.wav
ls -l "$out"
