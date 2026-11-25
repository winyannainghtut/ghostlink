// Text helpers: the letters of an avatar (shared/text.ts) and copying without a thrown error
// (shared/clipboard.ts), code review 2026-11-25, U12.
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { copyText } from '../shared/clipboard.ts';
import { initials } from '../shared/text.ts';

test('an avatar shows the first two characters, in capitals', () => {
  assert.equal(initials('Home A'), 'HO');
  assert.equal(initials('singapore edge'), 'SI');
  assert.equal(initials('x'), 'X');
  assert.equal(initials(''), '');
  assert.equal(initials('  padded'), 'PA');
});

test('an emoji or a flag at the start is not cut in half', () => {
  // title.slice(0, 2) kept one UTF-16 unit of the emoji here: a lone surrogate, shown as a box.
  assert.equal(initials('A😀 Tokyo'), 'A😀');
  assert.equal(initials('🇸🇬 Singapore'), '🇸🇬');
  assert.equal(initials('😀😀😀'), '😀😀');
  for (const name of ['A😀 Tokyo', '🇸🇬 Singapore', '😀 Home', '𝔊host']) assert.ok(!/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(initials(name)), name);
});

test('letters with accents and Burmese names stay whole', () => {
  assert.equal(initials('éa'), 'ÉA');
  // "e" and a combining acute accent are one letter on the screen.
  assert.equal(initials('éa'), 'ÉA');
  const burmese = initials('ကျွန်ုပ်၏ Home Lab');
  assert.ok(burmese.length >= 2);
  assert.ok(!burmese.endsWith('္'), 'a virama alone at the end means a cut cluster');
});

test('copying reports whether anything was copied, and never throws', async () => {
  const written = [];
  assert.equal(await copyText('key', { write: async text => void written.push(text) }), true);
  assert.deepEqual(written, ['key']);
  // The clipboard API refuses (the window lost focus): the older way is used.
  const legacy = [];
  assert.equal(await copyText('key', { write: async () => Promise.reject(new Error('Document is not focused')), legacy: text => (legacy.push(text), true) }), true);
  assert.deepEqual(legacy, ['key']);
  // The API is missing altogether (a page that is not a secure context): it throws at once.
  assert.equal(await copyText('key', { write: () => { throw new TypeError("Cannot read properties of undefined (reading 'writeText')"); }, legacy: () => true }), true);
  // Both fail: false, not an exception.
  assert.equal(await copyText('key', { write: async () => Promise.reject(new Error('no')), legacy: () => false }), false);
  assert.equal(await copyText('key', { write: async () => Promise.reject(new Error('no')), legacy: () => { throw new Error('no'); } }), false);
  assert.equal(await copyText('key', {}), false);
});
