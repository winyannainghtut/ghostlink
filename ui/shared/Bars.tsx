import { useState } from 'react';
import { visibleItems } from './chart';
import { bytes } from './format';
import { Meter, Skeleton, cx } from './glass';

// Horizontal bars for byte totals (traffic types, protocols, services, users). `undefined` items
// mean the numbers are still on their way (placeholders show); null or an empty list mean there is
// nothing to show (the `empty` text). Only the biggest `limit` rows show, with a "+N more" button
// for the rest rather than leaving them out without a word.
export function Bars({ items, empty, label, limit = 8 }: { items: { label: string; bytes: number }[] | null | undefined; empty: string; label?: (raw: string) => string; limit?: number }) {
  const [expanded, setExpanded] = useState(false);
  if (items === undefined) {
    return (
      <div role="status" aria-busy="true" className="grid grid-cols-1 gap-3 py-1">
        <span className="sr-only">Loading</span>
        {[0, 1, 2].map(i => (
          <div key={i} className="grid grid-cols-1 gap-1.5">
            <Skeleton className={cx('h-3.5', i === 0 ? 'w-2/5' : i === 1 ? 'w-1/3' : 'w-1/4')} />
            <Skeleton className="h-1.5 w-full" />
          </div>
        ))}
      </div>
    );
  }
  if (!items || !items.length) return <p className="py-6 text-center text-[13px] text-fg-3">{empty}</p>;
  const top = Math.max(1, ...items.map(i => i.bytes));
  const { shown, hidden } = visibleItems(items, limit, expanded);
  return (
    <div className="grid grid-cols-1 gap-3">
      <ul className="grid grid-cols-1 gap-3">
        {shown.map(item => (
          <li key={item.label} className="grid grid-cols-1 gap-1.5">
            <div className="flex items-baseline justify-between gap-3 text-[13px]">
              <span className="truncate text-fg">{label ? label(item.label) : item.label}</span>
              <span className="num shrink-0 text-fg-2">{bytes(item.bytes)}</span>
            </div>
            <Meter fraction={item.bytes / top} tone="down" />
          </li>
        ))}
      </ul>
      {(hidden > 0 || expanded) && (
        <button type="button" aria-expanded={expanded} onClick={() => setExpanded(!expanded)} className="justify-self-start rounded-full px-2.5 py-1 text-[12.5px] text-fg-2 transition hover:bg-glass-strong hover:text-fg">
          {expanded ? 'Show fewer' : `+${hidden} more`}
        </button>
      )}
    </div>
  );
}
