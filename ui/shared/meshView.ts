// The mesh map's view: zoom and pan, and where subnet pills go. The map draws its nodes in a fixed
// world (760 x 480 by default); the view is a scale `k` and an offset (x, y) in the map's own screen
// units, so a world point w is drawn at k * w + (x, y). Everything here is arithmetic: the map's
// pointer, wheel and keyboard handlers call it, and the tests run it directly. No imports (node
// --experimental-strip-types).

export type Point = { x: number; y: number };
export type View = { k: number; x: number; y: number };
export type Box = { minX: number; minY: number; maxX: number; maxY: number };
export type Rect = { x: number; y: number; w: number; h: number };

export const IDENTITY: View = { k: 1, x: 0, y: 0 };
export const MIN_SCALE = 0.3;
export const MAX_SCALE = 4;

export function clampScale(k: number): number {
  return Math.min(MAX_SCALE, Math.max(MIN_SCALE, Number.isFinite(k) ? k : 1));
}

// The world point drawn at screen point p, and the reverse.
export const toWorld = (v: View, p: Point): Point => ({ x: (p.x - v.x) / v.k, y: (p.y - v.y) / v.k });
export const toScreen = (v: View, p: Point): Point => ({ x: p.x * v.k + v.x, y: p.y * v.k + v.y });

// Zooms by `factor` around the screen point (px, py): the world point under it stays under it.
export function zoomAt(v: View, factor: number, px: number, py: number): View {
  const k = clampScale(v.k * (Number.isFinite(factor) && factor > 0 ? factor : 1));
  const ratio = k / v.k;
  return { k, x: px - (px - v.x) * ratio, y: py - (py - v.y) * ratio };
}

// How much one wheel event zooms. A mouse wheel notch reports about 100 (pixels), or 3 (lines);
// a touchpad pinch arrives as a wheel with ctrlKey and small deltas, so it is turned up. One event
// never zooms by more than a factor of 1.5, whatever the device reports.
export function wheelFactor(deltaY: number, deltaMode: number, ctrlKey: boolean): number {
  const pixels = deltaMode === 1 ? deltaY * 33 : deltaMode === 2 ? deltaY * 400 : deltaY;
  const factor = Math.exp(-pixels * (ctrlKey ? 0.01 : 0.0015));
  return Math.min(1.5, Math.max(1 / 1.5, Number.isFinite(factor) ? factor : 1));
}

// Two fingers: the view that keeps the world points under the fingers' previous positions under
// their new ones (the scale follows the distance between them, the offset follows their middle).
export function pinchView(v: View, prevA: Point, prevB: Point, nextA: Point, nextB: Point): View {
  const before = Math.hypot(prevB.x - prevA.x, prevB.y - prevA.y);
  const after = Math.hypot(nextB.x - nextA.x, nextB.y - nextA.y);
  const k = before > 0.5 && after > 0.5 ? clampScale(v.k * (after / before)) : v.k;
  const from = toWorld(v, { x: (prevA.x + prevB.x) / 2, y: (prevA.y + prevB.y) / 2 });
  const mid = { x: (nextA.x + nextB.x) / 2, y: (nextA.y + nextB.y) / 2 };
  return { k, x: mid.x - from.x * k, y: mid.y - from.y * k };
}

// Keeps some of the content in view: at least `margin` screen units of the content's box stay inside
// the window, so the map cannot be pushed away until it is gone.
export function clampView(v: View, box: Box | null, size: { w: number; h: number }, margin = 60): View {
  const k = clampScale(v.k);
  if (!box) return { k, x: v.x, y: v.y };
  const range = (offset: number, lo: number, hi: number, extent: number) => {
    const m = Math.min(margin, extent / 2);
    // The content's edges on screen are lo * k + offset and hi * k + offset.
    return Math.min(extent - m - lo * k, Math.max(m - hi * k, offset));
  };
  return { k, x: range(v.x, box.minX, box.maxX, size.w), y: range(v.y, box.minY, box.maxY, size.h) };
}

// The view that shows all of the content, centred, with `padding` around it. It never zooms in
// past `maxK`, so one node alone is not blown up to fill the map.
export function fitView(box: Box | null, size: { w: number; h: number }, padding = 24, maxK = 1.5): View {
  if (!box) return IDENTITY;
  const width = Math.max(1, box.maxX - box.minX);
  const height = Math.max(1, box.maxY - box.minY);
  const k = clampScale(Math.min(maxK, (size.w - 2 * padding) / width, (size.h - 2 * padding) / height));
  return { k, x: size.w / 2 - ((box.minX + box.maxX) / 2) * k, y: size.h / 2 - ((box.minY + box.maxY) / 2) * k };
}

export function boundsOf(rects: Rect[]): Box | null {
  if (!rects.length) return null;
  return {
    minX: Math.min(...rects.map(r => r.x)),
    minY: Math.min(...rects.map(r => r.y)),
    maxX: Math.max(...rects.map(r => r.x + r.w)),
    maxY: Math.max(...rects.map(r => r.y + r.h)),
  };
}

// --- subnet pills ------------------------------------------------------------------------------

export type PillNode = { id: string; x: number; y: number; name: string; routes: string[] };
export type Pill = Rect & { node: string; route: string; side: 'left' | 'right' };

const PILL_H = 18;
const PILL_STEP = 22; // a pill and the gap under it
const CHAR_W = 6.4;

export const pillWidth = (route: string): number => route.length * CHAR_W + 14;

// What a node covers: its shape, its name and its address (the labels are centred under it).
export function nodeBox(n: { x: number; y: number; name: string }, radius: number): Rect {
  const w = Math.max(2 * radius, n.name.length * 7 + 8);
  return { x: n.x - w / 2, y: n.y - radius - 4, w, h: 2 * radius + 36 };
}

const overlaps = (a: Rect, b: Rect, gap = 0) => a.x < b.x + b.w + gap && b.x < a.x + a.w + gap && a.y < b.y + b.h + gap && b.y < a.y + a.h + gap;
const inside = (r: Rect, area: { w: number; h: number }) => r.x >= 4 && r.y >= 4 && r.x + r.w <= area.w - 4 && r.y + r.h <= area.h - 4;
// Where a pill is tried, in turn: where it is wanted, then one pill-height down, one up, two down...
const SHIFTS = [0, 1, -1, 2, -2, 3, -3, 4, -4].map(n => n * PILL_STEP);

// Places the pills of every node beside it (the nodes without a subnet are given too, with no routes,
// so that no pill lands on them): outwards (a node in the right half has its pills on its
// right), a pill per route, stacked around the node's height. A pill that would cover another pill
// or a node, or leave `area` (the map's own window), is moved up or down, then to the other side of
// its node; when nothing inside the window is free it goes where it is wanted, outside if need be.
export function layoutPills(nodes: PillNode[], area: { w: number; h: number }, radius: number): Pill[] {
  const bodies = nodes.map(n => nodeBox(n, radius));
  const out: Pill[] = [];
  nodes.forEach((n, self) => {
    const preferred: 'left' | 'right' = n.x >= area.w / 2 ? 'right' : 'left';
    const sides: ('left' | 'right')[] = [preferred, preferred === 'right' ? 'left' : 'right'];
    n.routes.forEach((route, i) => {
      const w = pillWidth(route);
      const wanted = n.y - PILL_H / 2 + (i - (n.routes.length - 1) / 2) * PILL_STEP;
      const spot = (side: 'left' | 'right', shift: number): Pill => ({
        node: n.id,
        route,
        side,
        w,
        h: PILL_H,
        x: side === 'right' ? n.x + radius + 22 : n.x - radius - 22 - w,
        y: wanted + shift,
      });
      // A node's own pills start beyond its shape, so only the other nodes are in the way.
      const free = (p: Pill) => !out.some(o => overlaps(p, o, 3)) && !bodies.some((b, other) => other !== self && overlaps(p, b, 2));
      let pick: Pill | undefined;
      for (const keepInside of [true, false]) {
        for (const side of sides) {
          pick = SHIFTS.map(shift => spot(side, shift)).find(p => (!keepInside || inside(p, area)) && free(p));
          if (pick) break;
        }
        if (pick) break;
      }
      out.push(pick ?? spot(preferred, 0));
    });
  });
  return out;
}
