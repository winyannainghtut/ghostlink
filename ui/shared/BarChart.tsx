// Stacked usage bars per hour or day (download below upload), sized to the element.
import { useEffect, useId, useRef, useState } from 'react';
import { axisGutter, barTicks } from './chart';
import { bucketLabel, bytes } from './format';

export type UsageBucket = { t: number; down: number; up: number };

// The server returns only buckets that saw traffic; fill the whole period with empty ones so a
// single busy hour is one bar among 24 (or 168, 30, 90), not the whole chart. Buckets keep the
// server's alignment (its day starts), taken from the newest bucket.
export function fillBuckets(buckets: UsageBucket[], step: number, count: number, now = Date.now() / 1000): UsageBucket[] {
  const offset = buckets.length ? ((buckets[buckets.length - 1].t % step) + step) % step : 0;
  const end = Math.floor((now - offset) / step) * step + offset;
  const byT = new Map(buckets.map(b => [b.t, b]));
  const out: UsageBucket[] = [];
  for (let i = count - 1; i >= 0; i--) {
    const t = end - i * step;
    out.push(byT.get(t) ?? { t, down: 0, up: 0 });
  }
  return out;
}

export function BarChart({ buckets, unit, height = 170 }: { buckets: UsageBucket[]; unit: 'hour' | 'day'; height?: number }) {
  const box = useRef<HTMLDivElement>(null);
  // A chart's gradient ids must be its own: two charts on a page would paint with each other's.
  const uid = useId().replace(/[^A-Za-z0-9_-]/g, '');
  const [width, setWidth] = useState(640);
  // The bar under the pointer, and the one a click or tap chose: a finger lifts off the chart, which
  // would end a hover at once, so a tap keeps its readout until the next tap.
  const [hover, setHover] = useState<number | null>(null);
  const [pinned, setPinned] = useState<number | null>(null);
  useEffect(() => {
    const element = box.current;
    if (!element) return;
    const observer = new ResizeObserver(entries => setWidth(Math.max(240, Math.floor(entries[0].contentRect.width))));
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  useEffect(() => setPinned(null), [buckets.length, unit]);
  const top = 10, bottom = 22, plotH = height - top - bottom;
  const max = Math.max(1, ...buckets.map(b => b.down + b.up));
  // The scale's labels have a gutter of their own on the right, so no bar runs under one.
  const right = axisGutter([bytes(max), bytes(max / 2)], 8);
  const plotW = width - right;
  const slot = plotW / Math.max(buckets.length, 1);
  // More than a day of hourly bars (7 days is 168): an hour alone would not say which day.
  const withDate = unit === 'hour' && buckets.length > 24;
  const label = (t: number) => bucketLabel(t, unit, withDate);
  const ticks = barTicks(buckets.length, plotW, (unit === 'day' ? 6 : withDate ? 16 : 5) * 5.4);
  const quiet = !buckets.some(b => b.down + b.up > 0);
  const chosen = hover ?? pinned;
  const at = chosen !== null ? buckets[chosen] : null;
  const level = (fraction: number) => top + plotH - fraction * plotH;
  return (
    <div ref={box} className="relative">
      <svg className="chart block w-full" width={width} height={height} viewBox={`0 0 ${width} ${height}`} role="img" aria-label="Usage over time" onPointerLeave={() => setHover(null)}>
        <defs>
          <linearGradient id={`${uid}-down`} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" className="stop-down" stopOpacity="0.95" />
            <stop offset="1" className="stop-down" stopOpacity="0.45" />
          </linearGradient>
          <linearGradient id={`${uid}-up`} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" className="stop-up" stopOpacity="0.95" />
            <stop offset="1" className="stop-up" stopOpacity="0.5" />
          </linearGradient>
        </defs>
        {[0.5, 1].map(f => (
          <line key={f} className="gridline" x1={0} x2={plotW} y1={level(f)} y2={level(f)} strokeDasharray="2 4" />
        ))}
        <line className="gridline" x1={0} x2={plotW} y1={level(0)} y2={level(0)} />
        {buckets.map((b, i) => {
          const down = (b.down / max) * plotH, up = (b.up / max) * plotH, w = Math.max(1, slot - Math.min(4, slot * 0.3));
          const x = i * slot + (slot - w) / 2;
          return (
            <g key={b.t} onPointerEnter={() => setHover(i)} onClick={() => setPinned(current => (current === i ? null : i))} opacity={chosen === null || chosen === i ? 1 : 0.55}>
              <rect x={i * slot} y={top} width={slot} height={plotH} fill="transparent" />
              <rect x={x} y={top + plotH - down} width={w} height={down} rx={Math.min(3, w / 2)} fill={`url(#${uid}-down)`} />
              <rect x={x} y={top + plotH - down - up} width={w} height={up} rx={Math.min(3, w / 2)} fill={`url(#${uid}-up)`} />
            </g>
          );
        })}
        {buckets.length > 0 && (
          <>
            {ticks.map((i, k) => (
              <text key={i} className="axis-label" x={k === 0 ? 2 : k === ticks.length - 1 ? plotW - 2 : i * slot + slot / 2} y={height - 6} textAnchor={k === 0 ? 'start' : k === ticks.length - 1 ? 'end' : 'middle'}>
                {label(buckets[i].t)}
              </text>
            ))}
            {!quiet && (
              <>
                <text className="axis-label" x={width - 4} y={level(1)} textAnchor="end" dominantBaseline="central">
                  {bytes(max)}
                </text>
                <text className="axis-label" x={width - 4} y={level(1) + 13} textAnchor="end" dominantBaseline="central">
                  per {unit}
                </text>
                <text className="axis-label" x={width - 4} y={level(0.5)} textAnchor="end" dominantBaseline="central">
                  {bytes(max / 2)}
                </text>
              </>
            )}
          </>
        )}
      </svg>
      {quiet && <p className="pointer-events-none absolute inset-0 grid place-items-center text-[13px] text-fg-3">No traffic recorded in this period</p>}
      {at && (
        <div className="glass-strong pointer-events-none absolute top-1 left-2 flex gap-3 rounded-full px-3 py-1.5 text-[12px]">
          <span className="text-fg-3">{label(at.t)}</span>
          <span className="num text-down">↓ {bytes(at.down)}</span>
          <span className="num text-up">↑ {bytes(at.up)}</span>
        </div>
      )}
    </div>
  );
}
