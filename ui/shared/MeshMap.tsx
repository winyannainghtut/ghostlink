// The mesh map: nodes on a ring (relays first), every link between them, relayed links bent
// through the relay, shared subnets beside their node. Nodes can be dragged; positions are kept
// per mesh in localStorage. The map zooms (wheel, pinch, the buttons, + and -) around the pointer
// and pans (drag the background, or the arrow keys); 0 and the fit button show everything again.
// The arithmetic is in meshView.ts. Shared by the dashboard and the desktop app.
import { Maximize, Minus, Plus } from 'lucide-react';
import { useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type PointerEvent as ReactPointerEvent } from 'react';
import { cx, IconButton } from './glass';
import { speed } from './format';
import { boundsOf, clampView, fitView, IDENTITY, layoutPills, MAX_SCALE, MIN_SCALE, nodeBox, pinchView, toWorld, wheelFactor, zoomAt, type Pill, type Point, type View } from './meshView';

export type MeshNode = {
  id: string;
  name: string;
  address: string;
  tags: string[] | null;
  status: 'active' | 'pending' | 'disabled' | string;
  relay: boolean;
  relay_in_use: boolean;
  online: boolean;
  last_seen_unix: number;
  endpoint: string;
  seen_from: string;
  listen_port: number;
  advertised_routes: string[] | null;
  approved_routes: string[] | null;
  masquerade: boolean;
  version: string;
  os: string;
  error?: string;
  in_sync: boolean;
  rx_bps: number;
  tx_bps: number;
};

export type MeshLink = { a: string; b: string; path: 'direct' | 'relay' | 'down'; via?: string; rtt_ms: number; handshake_age_s: number; rx_bps: number; tx_bps: number };

export type Topology = { id: string; name: string; cidr: string; enabled: boolean; nodes: MeshNode[]; links: MeshLink[]; rule_count: number; updated_unix: number };

const W = 760, H = 480, R = 24;
const SIZE = { w: W, h: H };
// One press of a zoom button or of + and -; the arrow keys move the map by this much.
const STEP = 1.25;
const NUDGE = 40;
type Position = { x: number; y: number; pinned: boolean };

function ring(nodes: MeshNode[], saved: Record<string, { x: number; y: number }>): Record<string, Position> {
  const rank = (n: MeshNode) => (n.relay ? 0 : n.status === 'active' ? 1 : 2);
  const ordered = [...nodes].sort((a, b) => rank(a) - rank(b) || Number(a.id) - Number(b.id));
  const out: Record<string, Position> = {};
  const twoRings = ordered.length > 14;
  ordered.forEach((n, i) => {
    if (saved[n.id]) {
      out[n.id] = { ...saved[n.id], pinned: true };
      return;
    }
    if (ordered.length === 1) {
      out[n.id] = { x: W / 2, y: H / 2 - 20, pinned: false };
      return;
    }
    const scale = twoRings && i % 2 ? 0.6 : 1;
    const angle = -Math.PI / 2 + (2 * Math.PI * i) / ordered.length;
    out[n.id] = { x: W / 2 + (W / 2 - 130) * scale * Math.cos(angle), y: H / 2 - 12 + (H / 2 - 85) * scale * Math.sin(angle), pinned: false };
  });
  return out;
}

const storageKey = (id: string) => 'ghostlink-mesh-layout:' + id;

function loadSaved(id: string): Record<string, { x: number; y: number }> {
  try {
    return JSON.parse(localStorage.getItem(storageKey(id)) || '{}');
  } catch {
    return {};
  }
}

export function MeshMap({ topology, selected, onSelect, resetToken = 0, className }: {
  topology: Topology;
  selected: string | null;
  onSelect: (id: string | null) => void;
  resetToken?: number;
  className?: string;
}) {
  const svg = useRef<SVGSVGElement>(null);
  const ids = useMemo(() => topology.nodes.map(n => n.id).sort().join(','), [topology.nodes]);
  const [positions, setPositions] = useState<Record<string, Position>>({});
  const drag = useRef<{ id: string; start: Point; origin: Position; moved: boolean } | null>(null);
  const latest = useRef(positions);
  latest.current = positions;
  const handledReset = useRef(resetToken);

  // Zoom and pan. `view` is what is drawn; the handlers read the newest through the ref.
  const [view, setView] = useState<View>(IDENTITY);
  const viewRef = useRef(view);
  viewRef.current = view;
  // The pointers that are down on the map (a second one makes a pinch), what a drag of the
  // background started from, and whether it has moved enough to be a pan and not a click.
  const pointers = useRef(new Map<number, Point>());
  const pan = useRef<{ start: Point; view: View; moved: boolean } | null>(null);
  const [panning, setPanning] = useState(false);

  // Lay out again only when nodes come or go (or on "Rearrange"); dragged nodes keep their place.
  // Only a new "Rearrange" forgets the saved layout: the token stays set after it, and nodes
  // dragged since then must survive the next node that joins or leaves.
  useEffect(() => {
    const reset = resetToken !== handledReset.current;
    handledReset.current = resetToken;
    if (reset) {
      try {
        localStorage.removeItem(storageKey(topology.id));
      } catch {
        /* storage off */
      }
    }
    setPositions(ring(topology.nodes, reset ? {} : loadSaved(topology.id)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids, topology.id, resetToken]);
  // Another mesh, or "Rearrange": the whole map again. (Nodes coming and going keep the view.)
  useEffect(() => setView(IDENTITY), [topology.id, resetToken]);

  // Where the pills go (beside their nodes, not over each other), and what the content covers.
  const placed = useMemo(() => topology.nodes.filter(n => positions[n.id]), [topology.nodes, positions]);
  const pills = useMemo(
    // Every node is given, also those without a subnet: a pill must not land on any of them.
    () => layoutPills(placed.map(n => ({ id: n.id, x: positions[n.id].x, y: positions[n.id].y, name: n.name, routes: n.approved_routes ?? [] })), SIZE, R),
    [placed, positions],
  );
  const bounds = useMemo(() => boundsOf([...placed.map(n => nodeBox({ ...positions[n.id], name: n.name }, R)), ...pills]), [placed, positions, pills]);
  const boundsRef = useRef(bounds);
  boundsRef.current = bounds;
  const pillsByNode = useMemo(() => {
    const out: Record<string, Pill[]> = {};
    for (const p of pills) (out[p.node] ??= []).push(p);
    return out;
  }, [pills]);

  // The map's own screen units (the viewBox), whatever size it is drawn at.
  const point = (event: { clientX: number; clientY: number }): Point => {
    const matrix = svg.current?.getScreenCTM();
    if (!matrix) return { x: 0, y: 0 };
    const p = new DOMPoint(event.clientX, event.clientY).matrixTransform(matrix.inverse());
    return { x: p.x, y: p.y };
  };
  const world = (event: { clientX: number; clientY: number }) => toWorld(viewRef.current, point(event));
  const change = (next: (v: View) => View) => setView(v => clampView(next(v), boundsRef.current, SIZE));
  const zoomBy = (factor: number, around: Point = { x: W / 2, y: H / 2 }) => change(v => zoomAt(v, factor, around.x, around.y));
  const fit = () => setView(clampView(fitView(boundsRef.current, SIZE), boundsRef.current, SIZE));

  // The wheel zooms around the pointer. It needs its own listener: React's are passive, and a
  // passive one cannot stop the page from scrolling.
  const hasNodes = topology.nodes.length > 0;
  useEffect(() => {
    const element = svg.current;
    if (!element) return;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      const matrix = element.getScreenCTM();
      if (!matrix) return;
      const p = new DOMPoint(event.clientX, event.clientY).matrixTransform(matrix.inverse());
      setView(v => clampView(zoomAt(v, wheelFactor(event.deltaY, event.deltaMode, event.ctrlKey), p.x, p.y), boundsRef.current, SIZE));
    };
    element.addEventListener('wheel', onWheel, { passive: false });
    return () => element.removeEventListener('wheel', onWheel);
  }, [hasNodes]);

  const save = (next: Record<string, Position>) => {
    const out: Record<string, { x: number; y: number }> = {};
    for (const [id, p] of Object.entries(next)) if (p.pinned) out[id] = { x: Math.round(p.x), y: Math.round(p.y) };
    try {
      localStorage.setItem(storageKey(topology.id), JSON.stringify(out));
    } catch {
      /* storage off */
    }
  };

  const onKeyDown = (event: ReactKeyboardEvent) => {
    if (event.ctrlKey || event.metaKey || event.altKey) return;
    const actions: Record<string, () => void> = {
      '+': () => zoomBy(STEP),
      '=': () => zoomBy(STEP),
      '-': () => zoomBy(1 / STEP),
      _: () => zoomBy(1 / STEP),
      '0': fit,
      ArrowLeft: () => change(v => ({ ...v, x: v.x + NUDGE })),
      ArrowRight: () => change(v => ({ ...v, x: v.x - NUDGE })),
      ArrowUp: () => change(v => ({ ...v, y: v.y + NUDGE })),
      ArrowDown: () => change(v => ({ ...v, y: v.y - NUDGE })),
    };
    const act = actions[event.key];
    if (!act) return;
    event.preventDefault();
    act();
  };

  const wanted = (event: ReactPointerEvent) => event.pointerType !== 'mouse' || event.button === 0;
  const grab = (event: ReactPointerEvent) => {
    try {
      svg.current?.setPointerCapture(event.pointerId);
    } catch {
      /* synthetic pointer */
    }
  };
  const release = (event: ReactPointerEvent) => {
    pointers.current.delete(event.pointerId);
    setPanning(false);
    const d = drag.current;
    const p = pan.current;
    drag.current = null;
    pan.current = null;
    // Only a pointer let go is a click or the end of a drag; fingers still down after a pinch do
    // nothing more (their next move would be a jump).
    if (event.type !== 'pointerup' || pointers.current.size > 0) return;
    if (d) {
      if (d.moved) save(latest.current);
      else onSelect(d.id);
    } else if (p && !p.moved) onSelect(null);
  };

  const byId = Object.fromEntries(topology.nodes.map(n => [n.id, n]));
  const labelAll = topology.links.length <= 6;

  if (!topology.nodes.length) {
    return (
      <svg className={cx('mesh-map w-full', className)} viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Mesh map">
        <text className="empty" x={W / 2} y={H / 2} textAnchor="middle">
          No nodes yet. Add one to see the map.
        </text>
      </svg>
    );
  }

  return (
    <div className="relative" onKeyDown={onKeyDown}>
      <svg
        ref={svg}
        className={cx('mesh-map w-full', panning && 'panning', className)}
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="xMidYMid meet"
        role="group"
        aria-label="Mesh map: nodes and the links between them. Plus and minus zoom, 0 shows everything, the arrow keys move the map."
        tabIndex={0}
        onPointerDownCapture={event => {
          if (!wanted(event)) return;
          // A mouse is one pointer: one that was never let go (the window lost it) is forgotten.
          if (event.pointerType === 'mouse') pointers.current.clear();
          pointers.current.set(event.pointerId, point(event));
          // A second finger turns whatever the first was doing into a pinch.
          if (pointers.current.size === 2) {
            drag.current = null;
            pan.current = null;
            setPanning(false);
          }
        }}
        onPointerDown={event => {
          // Only the background gets here (a node stops its own pointerdown): a drag of it pans.
          if (!wanted(event) || pointers.current.size !== 1) return;
          pan.current = { start: point(event), view: viewRef.current, moved: false };
          grab(event);
        }}
        onPointerMove={event => {
          const was = pointers.current.get(event.pointerId);
          if (!was) return;
          const now = point(event);
          pointers.current.set(event.pointerId, now);
          if (pointers.current.size === 2) {
            const other = [...pointers.current].find(([id]) => id !== event.pointerId)?.[1];
            if (other) change(v => pinchView(v, was, other, now, other));
            return;
          }
          const d = drag.current;
          if (d) {
            const at = world(event);
            const dx = at.x - d.start.x, dy = at.y - d.start.y;
            if (!d.moved && Math.hypot(dx, dy) * viewRef.current.k < 4) return;
            d.moved = true;
            setPositions(current => ({ ...current, [d.id]: { x: Math.min(W - 30, Math.max(30, d.origin.x + dx)), y: Math.min(H - 45, Math.max(30, d.origin.y + dy)), pinned: true } }));
            return;
          }
          const p = pan.current;
          if (!p) return;
          if (!p.moved && Math.hypot(now.x - p.start.x, now.y - p.start.y) < 4) return;
          if (!p.moved) setPanning(true);
          p.moved = true;
          change(() => ({ k: p.view.k, x: p.view.x + now.x - p.start.x, y: p.view.y + now.y - p.start.y }));
        }}
        onPointerUp={release}
        onPointerCancel={release}
        onLostPointerCapture={release}
      >
        <g transform={`translate(${view.x.toFixed(2)} ${view.y.toFixed(2)}) scale(${view.k.toFixed(4)})`}>
          <g>
            {topology.links.map(link => {
              const a = positions[link.a], b = positions[link.b];
              if (!a || !b) return null;
              const busy = (link.rx_bps || 0) + (link.tx_bps || 0);
              const width = 1.5 + Math.min(4.5, Math.log10(1 + busy / 1000));
              const isSelected = !!selected && (link.a === selected || link.b === selected);
              const relay = link.path === 'relay' && link.via ? positions[link.via] : undefined;
              const cls = cx('link', link.path, isSelected && 'selected');
              let d: string, mid: { x: number; y: number };
              if (relay) {
                // This control point makes the curve pass through the relay at its middle.
                const cx2 = 2 * relay.x - (a.x + b.x) / 2, cy2 = 2 * relay.y - (a.y + b.y) / 2;
                d = `M${a.x},${a.y} Q${cx2},${cy2} ${b.x},${b.y}`;
                mid = { x: 0.5625 * a.x + 0.375 * cx2 + 0.0625 * b.x, y: 0.5625 * a.y + 0.375 * cy2 + 0.0625 * b.y };
              } else {
                d = `M${a.x},${a.y} L${b.x},${b.y}`;
                mid = { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
              }
              const via = relay && link.via ? ` via ${byId[link.via]?.name}` : '';
              return (
                <g key={link.a + '-' + link.b}>
                  <path className={cls} d={d} strokeWidth={width.toFixed(1)}>
                    <title>{`${byId[link.a]?.name} ↔ ${byId[link.b]?.name}: ${link.path}${via}, ${link.rtt_ms > 0 ? link.rtt_ms.toFixed(1) + ' ms' : 'no ping yet'}, ${speed(busy)}`}</title>
                  </path>
                  {link.rtt_ms > 0 && (labelAll || isSelected) && (
                    <text className="label" x={mid.x} y={mid.y - 5} textAnchor="middle">
                      {link.rtt_ms < 10 ? link.rtt_ms.toFixed(1) : Math.round(link.rtt_ms)} ms
                    </text>
                  )}
                </g>
              );
            })}
          </g>
          <g>
            {topology.nodes.map(node => {
              const p = positions[node.id];
              if (!p) return null;
              const routes = node.approved_routes || [];
              const state = node.status !== 'active' ? node.status : node.online ? 'online' : 'offline';
              const tip = [`${node.name} (${node.address}) · ${state}`];
              if (node.relay_in_use) tip.push('Relay for nodes that cannot connect directly');
              if (node.tags?.length) tip.push('Tags: ' + node.tags.join(', '));
              if (node.error) tip.push('Problem: ' + node.error);
              return (
                <g key={node.id}>
                  {(pillsByNode[node.id] ?? []).map(pill => (
                    <g key={pill.route}>
                      <line className="subnet-link" x1={p.x + (pill.side === 'right' ? R : -R)} y1={p.y} x2={pill.side === 'right' ? pill.x : pill.x + pill.w} y2={pill.y + pill.h / 2} />
                      <g className="subnet">
                        <rect x={pill.x} y={pill.y} width={pill.w} height={pill.h} rx={pill.h / 2} />
                        <text x={pill.x + 7} y={pill.y + 13}>
                          {pill.route}
                        </text>
                      </g>
                    </g>
                  ))}
                  <g
                    className={cx('node', state, node.id === selected && 'selected')}
                    transform={`translate(${p.x} ${p.y})`}
                    tabIndex={0}
                    role="button"
                    aria-label={`${node.name}, ${state}`}
                    onPointerDown={event => {
                      event.preventDefault();
                      event.stopPropagation();
                      if (!wanted(event)) return;
                      // A pinch (a second finger already down) is not a drag of this node.
                      if (pointers.current.size === 1) drag.current = { id: node.id, start: world(event), origin: { ...p }, moved: false };
                      grab(event);
                    }}
                    onKeyDown={event => {
                      if (event.key === 'Enter' || event.key === ' ') {
                        event.preventDefault();
                        onSelect(node.id);
                      }
                    }}
                  >
                    <title>{tip.join('\n')}</title>
                    {node.relay_in_use && <circle className="relay-ring" r={R + 6} />}
                    {routes.length ? <rect className="shape" x={-R} y={-R} width={2 * R} height={2 * R} rx={9} /> : <circle className="shape" r={R} />}
                    <text className="glyph" y={5} textAnchor="middle">
                      {node.name.slice(0, 1).toUpperCase()}
                    </text>
                    <text className="name" y={R + 16} textAnchor="middle">
                      {node.name}
                    </text>
                    <text className="address" y={R + 29} textAnchor="middle">
                      {node.status === 'pending' ? 'waiting for approval' : node.address}
                    </text>
                  </g>
                </g>
              );
            })}
          </g>
        </g>
      </svg>
      <div role="group" aria-label="Zoom the map" className="inset-glass absolute top-3 right-3 flex items-center gap-0.5 rounded-full p-0.5">
        <IconButton label="Zoom out" disabled={view.k <= MIN_SCALE + 1e-6} onClick={() => zoomBy(1 / STEP)}>
          <Minus size={16} />
        </IconButton>
        <span className="num w-11 text-center text-[12px] text-fg-2" title="Zoom">
          {Math.round(view.k * 100)}%
        </span>
        <IconButton label="Zoom in" disabled={view.k >= MAX_SCALE - 1e-6} onClick={() => zoomBy(STEP)}>
          <Plus size={16} />
        </IconButton>
        <IconButton label="Show the whole map" onClick={fit}>
          <Maximize size={15} />
        </IconButton>
      </div>
    </div>
  );
}
