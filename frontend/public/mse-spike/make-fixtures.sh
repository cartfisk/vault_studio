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
ffmpeg -v error -i "$out/source.wav" -t 10 -c copy "$out/h1.wav"
ffmpeg -v error -ss 10 -i "$out/source.wav" -c copy "$out/h2.wav"

for half in h1 h2; do
  ffmpeg -v error -i "$out/$half.wav" -c:a aac -b:a 256k \
    -movflags "$frag" -f mp4 "$out/$half-aac.mp4"
  ffmpeg -v error -i "$out/$half.wav" -c:a alac \
    -movflags "$frag" -f mp4 "$out/$half-alac.mp4"
done

# A pure tone compresses to almost nothing in ALAC, which would not exercise
# the memory question at all. Noise is incompressible, so this approximates a
# real lossless track's buffer footprint.
ffmpeg -v error -f lavfi -i "anoisesrc=r=44100:d=240:c=pink" -ac 2 \
  -c:a alac -movflags "$frag" -f mp4 "$out/big-alac.mp4"

rm -f "$out"/*.wav
ls -l "$out"
