/**
 * Where an append lands on the shared timeline, and how much of it to keep.
 *
 * Both encoders overshoot the true content: AAC adds priming at the head and
 * padding at the tail, ALAC pads its final frame out to 4096 samples. The
 * container reports the padded duration, so appending untrimmed inserts that
 * overshoot as garbage between tracks. Append windows are expressed in
 * presentation-timeline seconds, so they carry the offset.
 */
export function computeAppendPlan({ trueDuration, index }) {
  const start = trueDuration * index;
  return {
    timestampOffset: start,
    appendWindowStart: start,
    appendWindowEnd: start + trueDuration,
  };
}
