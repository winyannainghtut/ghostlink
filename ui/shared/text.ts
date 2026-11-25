// Text helpers with no imports, so the tests run them directly (node --experimental-strip-types).

// The first letters of a name, for a round avatar: the first two characters as a person sees them
// (a flag or an emoji at the start is one, whatever its length in UTF-16 units; so is a letter
// with its accent or a Burmese consonant with its signs). slice(0, 2) on the string would cut a
// character in half and leave a broken glyph.
export function initials(text: string, count = 2): string {
  const trimmed = text.trim();
  const parts = typeof Intl.Segmenter === 'function' ? Array.from(new Intl.Segmenter(undefined, { granularity: 'grapheme' }).segment(trimmed), s => s.segment) : Array.from(trimmed);
  return parts.slice(0, count).join('').trim().toUpperCase();
}
