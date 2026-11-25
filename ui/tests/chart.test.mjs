// How the charts and bars use their room (shared/chart.ts).
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { axisGutter, barTicks, visibleItems } from '../shared/chart.ts';

test('a chart keeps room on its right for its widest tick label', () => {
  // The labels the live chart draws, from "250 KB/s" to "100.0 MB/s": each is narrower than the
  // gutter, which is what keeps it off the curve (about 5.3 px per character at 10 px).
  for (const labels of [['250 KB/s', '500 KB/s'], ['5.0 MB/s', '10.0 MB/s'], ['25.0 MB/s', '100.0 MB/s']]) {
    const gutter = axisGutter(labels);
    for (const label of labels) assert.ok(gutter >= label.length * 5.3 + 8, `${label} needs more than ${gutter} px`);
  }
});

test('the gutter does not change width while the scale stays within a few characters', () => {
  assert.equal(axisGutter(['250 KB/s']), axisGutter(['10.0 MB/s']));
  assert.ok(axisGutter(['100.0 MB/s']) > axisGutter(['10.0 MB/s']));
});

test('a long usage chart is labelled along its bars, not only at its two ends', () => {
  const room = 640, width = 86, slot = room / 168;
  const ticks = barTicks(168, room, width);
  assert.ok(ticks.length >= 4, `only ${ticks.length} labels`);
  assert.equal(ticks[0], 0);
  assert.equal(ticks[ticks.length - 1], 167);
  assert.deepEqual(ticks, [...ticks].sort((a, b) => a - b));
  // Where each label sits, as the chart draws it: the first from the left edge, the last to the right
  // edge, the rest centred under their bar. No two share room (the old spacing let the first two touch).
  const boxes = ticks.map((i, k) => (k === 0 ? [2, 2 + width] : k === ticks.length - 1 ? [room - 2 - width, room - 2] : [i * slot + slot / 2 - width / 2, i * slot + slot / 2 + width / 2]));
  for (let k = 1; k < boxes.length; k++) assert.ok(boxes[k][0] - boxes[k - 1][1] >= 10, `labels ${k - 1} and ${k} are ${boxes[k][0] - boxes[k - 1][1]} px apart`);
});

test('a narrow or tiny chart still labels its first and last bar', () => {
  assert.deepEqual(barTicks(0, 640, 40), []);
  assert.deepEqual(barTicks(1, 640, 40), [0]);
  assert.deepEqual(barTicks(2, 100, 80), [0, 1]);
  assert.deepEqual(barTicks(30, 200, 80), [0, 29]);
  assert.ok(barTicks(24, 2000, 40).length <= 8);
  // Short labels (hours of a day) get more of them than long ones (a date and a time).
  assert.ok(barTicks(24, 640, 27).length > barTicks(168, 640, 86).length - 1);
});

test('bars that do not fit say how many are left out instead of dropping them', () => {
  const rows = Array.from({ length: 12 }, (_, i) => i);
  assert.deepEqual(visibleItems(rows, 8, false), { shown: rows.slice(0, 8), hidden: 4 });
  assert.deepEqual(visibleItems(rows, 8, true), { shown: rows, hidden: 0 });
  assert.deepEqual(visibleItems(rows.slice(0, 8), 8, false), { shown: rows.slice(0, 8), hidden: 0 });
  assert.deepEqual(visibleItems([], 8, false), { shown: [], hidden: 0 });
});
