// The mesh map's zoom, pan and subnet pills (shared/meshView.ts), code review 2026-11-25, U10: a dense
// topology could not be zoomed or panned, and the pills of nodes dragged close together overlapped.
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { boundsOf, clampScale, clampView, fitView, IDENTITY, layoutPills, MAX_SCALE, MIN_SCALE, nodeBox, pillWidth, pinchView, toScreen, toWorld, wheelFactor, zoomAt } from '../shared/meshView.ts';

const SIZE = { w: 760, h: 480 };
const near = (a, b, message) => assert.ok(Math.abs(a - b) < 1e-9, `${message ?? ''} ${a} vs ${b}`);

test('zooming around a point keeps the world point under it where it was', () => {
  let view = IDENTITY;
  const pointer = { x: 600, y: 120 };
  const under = toWorld(view, pointer);
  for (const factor of [1.25, 1.25, 0.8, 1.4, 0.5]) {
    view = zoomAt(view, factor, pointer.x, pointer.y);
    const now = toWorld(view, pointer);
    near(now.x, under.x, `x after ${factor}`);
    near(now.y, under.y, `y after ${factor}`);
  }
  // From a view that is already panned and zoomed.
  const start = { k: 2, x: -300, y: -80 };
  const world = toWorld(start, { x: 100, y: 400 });
  const after = zoomAt(start, 1.5, 100, 400);
  near(toWorld(after, { x: 100, y: 400 }).x, world.x);
  near(toWorld(after, { x: 100, y: 400 }).y, world.y);
  near(after.k, 3);
  const there = toScreen(after, world);
  near(there.x, 100);
  near(there.y, 400);
});

test('the scale stays between its limits, however far it is pushed', () => {
  let view = IDENTITY;
  for (let i = 0; i < 50; i++) view = zoomAt(view, 1.25, 380, 240);
  assert.equal(view.k, MAX_SCALE);
  for (let i = 0; i < 100; i++) view = zoomAt(view, 0.8, 380, 240);
  assert.equal(view.k, MIN_SCALE);
  assert.equal(clampScale(NaN), 1);
  assert.equal(clampScale(Infinity), 1, 'not a scale: the view stays as it is');
  // A factor that is not a number changes nothing.
  assert.deepEqual(zoomAt(IDENTITY, NaN, 1, 1), IDENTITY);
  assert.deepEqual(zoomAt(IDENTITY, -2, 1, 1), IDENTITY);
  // At the limit the world point still stays where it was: the view does not drift while pinned.
  const p = { x: 200, y: 300 };
  const pinned = zoomAt({ k: MAX_SCALE, x: -500, y: -300 }, 2, p.x, p.y);
  assert.deepEqual(pinned, { k: MAX_SCALE, x: -500, y: -300 });
});

test('a wheel notch zooms by a step, a touchpad pinch by a little, and never by a jump', () => {
  const notch = wheelFactor(-100, 0, false);
  assert.ok(notch > 1.1 && notch < 1.25, String(notch));
  near(wheelFactor(100, 0, false) * notch, 1, 'a notch up and a notch down cancel');
  const pinch = wheelFactor(-3, 0, true);
  assert.ok(pinch > 1.02 && pinch < 1.1, String(pinch));
  // A wheel that counts lines, and one with a huge delta.
  assert.ok(wheelFactor(-3, 1, false) > 1.05);
  assert.equal(wheelFactor(-100000, 0, false), 1.5);
  near(wheelFactor(100000, 0, false), 1 / 1.5);
  assert.equal(wheelFactor(NaN, 0, false), 1);
});

test('two fingers zoom by the distance between them and move the map with their middle', () => {
  const view = { k: 1, x: 0, y: 0 };
  // Fingers spread to twice the distance around the same middle: twice the scale, the middle stays put.
  const a = { x: 300, y: 240 }, b = { x: 460, y: 240 };
  const spread = pinchView(view, a, b, { x: 260, y: 240 }, { x: 500, y: 240 });
  near(spread.k, 1.5);
  const middle = { x: 380, y: 240 };
  near(toWorld(spread, middle).x, toWorld(view, middle).x);
  near(toWorld(spread, middle).y, toWorld(view, middle).y);
  // Both fingers slide by (50, -20) without changing their distance: the map pans by that much.
  const slid = pinchView(view, a, b, { x: 350, y: 220 }, { x: 510, y: 220 });
  near(slid.k, 1);
  near(slid.x, 50);
  near(slid.y, -20);
  // Fingers on the same spot (no distance) do not make a scale of infinity or zero.
  assert.equal(pinchView(view, a, a, a, b).k, 1);
});

test('the map cannot be pushed away until none of it is left in view', () => {
  const box = { minX: 100, minY: 80, maxX: 660, maxY: 420 };
  const far = clampView({ k: 1, x: 5000, y: -5000 }, box, SIZE);
  // The content's left edge is at most (window - margin) from the left of the window...
  assert.ok(box.minX * far.k + far.x <= SIZE.w - 60 + 1e-9);
  // ...and the bottom edge at least `margin` below the top.
  assert.ok(box.maxY * far.k + far.y >= 60 - 1e-9);
  const away = clampView({ k: 1, x: -5000, y: 5000 }, box, SIZE);
  assert.ok(box.maxX * away.k + away.x >= 60 - 1e-9);
  assert.ok(box.minY * away.k + away.y <= SIZE.h - 60 + 1e-9);
  // A view that is fine is not touched, and the scale is clamped with it.
  assert.deepEqual(clampView({ k: 1, x: -40, y: 25 }, box, SIZE), { k: 1, x: -40, y: 25 });
  assert.equal(clampView({ k: 99, x: 0, y: 0 }, box, SIZE).k, MAX_SCALE);
  assert.deepEqual(clampView({ k: 2, x: 7, y: 9 }, null, SIZE), { k: 2, x: 7, y: 9 });
});

test('fit shows all of the content, centred, without blowing up a single node', () => {
  const box = { minX: 60, minY: 70, maxX: 900, maxY: 400 };
  const view = fitView(box, SIZE, 24);
  // All four corners are inside the window, with the padding.
  for (const [x, y] of [[box.minX, box.minY], [box.maxX, box.maxY]]) {
    const at = toScreen(view, { x, y });
    assert.ok(at.x >= 24 - 1e-9 && at.x <= SIZE.w - 24 + 1e-9, `x ${at.x}`);
    assert.ok(at.y >= 24 - 1e-9 && at.y <= SIZE.h - 24 + 1e-9, `y ${at.y}`);
  }
  const centre = toScreen(view, { x: (box.minX + box.maxX) / 2, y: (box.minY + box.maxY) / 2 });
  near(centre.x, SIZE.w / 2);
  near(centre.y, SIZE.h / 2);
  assert.ok(view.k < 1, 'content wider than the window needs zooming out');
  // One node alone: not zoomed past 1.5, but centred.
  const alone = fitView({ minX: 356, minY: 192, maxX: 404, maxY: 264 }, SIZE);
  assert.equal(alone.k, 1.5);
  assert.deepEqual(fitView(null, SIZE), IDENTITY);
});

test('the content box covers every rectangle', () => {
  assert.equal(boundsOf([]), null);
  assert.deepEqual(boundsOf([{ x: 10, y: 20, w: 30, h: 40 }, { x: -5, y: 0, w: 5, h: 100 }]), { minX: -5, minY: 0, maxX: 40, maxY: 100 });
});

// --- subnet pills --------------------------------------------------------------------------------

const overlap = (a, b) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
const R = 24;

test('a node\'s pill sits beside it, outwards, at its height', () => {
  const [left] = layoutPills([{ id: '1', x: 200, y: 200, name: 'berlin', routes: ['192.0.2.0/24'] }], SIZE, R);
  assert.equal(left.side, 'left');
  assert.ok(left.x + left.w < 200 - R, 'to the left of the node');
  near(left.y + left.h / 2, 200);
  const [right] = layoutPills([{ id: '2', x: 500, y: 200, name: 'oslo', routes: ['198.51.100.0/24'] }], SIZE, R);
  assert.equal(right.side, 'right');
  assert.ok(right.x > 500 + R);
  assert.equal(right.w, pillWidth('198.51.100.0/24'));
});

test('several pills of one node are stacked, not on top of each other', () => {
  const pills = layoutPills([{ id: '1', x: 300, y: 240, name: 'berlin', routes: ['192.0.2.0/24', '198.51.100.0/24', '203.0.113.0/24'] }], SIZE, R);
  assert.equal(pills.length, 3);
  for (let i = 0; i < pills.length; i++) for (let j = i + 1; j < pills.length; j++) assert.ok(!overlap(pills[i], pills[j]), `${i} and ${j}`);
});

test('the pills of nodes dragged close together do not overlap each other or the other node', () => {
  // Two nodes 30 units apart, each with a subnet: the pills of both want the same place.
  const nodes = [
    { id: '1', x: 300, y: 200, name: 'alpha', routes: ['192.0.2.0/24', '192.0.2.128/25'] },
    { id: '2', x: 330, y: 215, name: 'bravo', routes: ['198.51.100.0/24'] },
    { id: '3', x: 270, y: 190, name: 'charlie', routes: ['203.0.113.0/24'] },
  ];
  const pills = layoutPills(nodes, SIZE, R);
  assert.equal(pills.length, 4);
  for (let i = 0; i < pills.length; i++) {
    for (let j = i + 1; j < pills.length; j++) assert.ok(!overlap(pills[i], pills[j]), `${pills[i].route} and ${pills[j].route}`);
    for (const node of nodes) if (node.id !== pills[i].node) assert.ok(!overlap(pills[i], nodeBox(node, R)), `${pills[i].route} covers ${node.name}`);
  }
});

test('a node without a subnet is in the way of the pills of others', () => {
  // Quito has three subnets and Delhi, to its right, none: the pills must not land on Delhi's name.
  const nodes = [
    { id: '1', x: 520, y: 480 - 90, name: 'quito-ci', routes: ['203.0.113.0/24', '198.51.100.64/26', '192.0.2.200/29'] },
    { id: '2', x: 640, y: 480 - 110, name: 'delhi-api', routes: [] },
  ];
  const pills = layoutPills(nodes, SIZE, R);
  assert.equal(pills.length, 3);
  for (const pill of pills) assert.ok(!overlap(pill, nodeBox(nodes[1], R)), `${pill.route} covers delhi-api`);
});

test('a pill that would run out of the window goes to the other side of its node', () => {
  // A node at the right edge of the ring: its pill on the right would end past the window.
  const [pill] = layoutPills([{ id: '1', x: 630, y: 240, name: 'tokyo', routes: ['192.0.2.0/24'] }], SIZE, R);
  assert.ok(pill.x + pill.w <= SIZE.w - 4, `ends at ${pill.x + pill.w}`);
  assert.equal(pill.side, 'left');
  // Where nothing fits (a window that is tiny) it still goes beside its node, outside if need be.
  const [cramped] = layoutPills([{ id: '1', x: 40, y: 40, name: 'tokyo', routes: ['192.0.2.0/24'] }], { w: 60, h: 60 }, R);
  assert.ok(cramped);
});
