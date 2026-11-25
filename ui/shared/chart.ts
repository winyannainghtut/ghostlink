// Layout arithmetic of the charts and bars, with no imports so the tests run it directly
// (node --experimental-strip-types).

// The room a chart keeps on its right for the tick labels, so a label never sits on the curve.
// A label of the chart's 10 px type takes about 5.3 px per character; the gutter is for at least
// `minChars` characters, so it does not change width when the scale crosses from "999 KB/s" to
// "1.0 MB/s" or "10.0 MB/s" and the whole curve shifts with it.
export function axisGutter(labels: string[], minChars = 9, charWidth = 5.6, pad = 10): number {
  return Math.ceil(Math.max(minChars, ...labels.map(l => l.length)) * charWidth) + pad;
}

// Which of `count` bars get a label under them: the first and the last, and evenly spaced ones
// between them as far as `room` px allows with labels `labelWidth` px wide. The first label starts
// at the left edge and the last ends at the right edge, while the ones between are centred under
// their bar, so those must clear both ends by a label and a gap (at most six between).
export function barTicks(count: number, room: number, labelWidth: number): number[] {
  if (count <= 0) return [];
  if (count === 1) return [0];
  const gap = 16, slot = room / count;
  const lo = 1.5 * labelWidth + gap, hi = room - 1.5 * labelWidth - gap;
  const out = [0];
  if (hi >= lo) {
    const slots = Math.min(6, Math.floor((hi - lo) / (labelWidth + gap)) + 1);
    for (let k = 0; k < slots; k++) {
      const centre = slots === 1 ? (lo + hi) / 2 : lo + (k * (hi - lo)) / (slots - 1);
      const index = Math.min(count - 2, Math.max(1, Math.round((centre - slot / 2) / slot)));
      if (index > out[out.length - 1] && index < count - 1) out.push(index);
    }
  }
  out.push(count - 1);
  return out;
}

// The rows a list of bars shows: all of them when expanded, else the first `limit`, with how many
// are left out so the list can say "+N more" instead of dropping them without a word.
export function visibleItems<T>(items: readonly T[], limit: number, expanded: boolean): { shown: T[]; hidden: number } {
  if (expanded || items.length <= limit) return { shown: [...items], hidden: 0 };
  return { shown: items.slice(0, limit), hidden: items.length - limit };
}
