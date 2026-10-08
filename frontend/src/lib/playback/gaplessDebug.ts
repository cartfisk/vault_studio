/** Opt-in gapless diagnostics: `localStorage.gaplessDebug = "1"`. */
export function gaplessDebug(): boolean {
  try {
    return localStorage.getItem("gaplessDebug") === "1";
  } catch {
    return false;
  }
}

export function rangesOf(el: HTMLMediaElement | null | undefined): number[][] {
  return el
    ? Array.from({ length: el.buffered.length }, (_, i) => [
        el.buffered.start(i),
        el.buffered.end(i),
      ])
    : [];
}
