// A small frosted panel anchored to a button. It renders at the page level: a frosted element
// cannot blur through a frosted parent (the card or nav it opens from). For the keyboard: opening
// it puts the focus on its first item (or the one marked data-autofocus), the arrow keys (and Home and End) move between its buttons,
// Escape closes it, and closing it from the keyboard returns the focus to the button it opened from.
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { cx } from './glass';

const ITEMS = '[role=menuitem], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled)';

export function Popover({ anchor, open, onClose, children, width = 240, align = 'end', className, role, label }: {
  anchor: RefObject<HTMLElement | null>;
  open: boolean;
  onClose: () => void;
  children: ReactNode;
  width?: number;
  align?: 'start' | 'center' | 'end';
  className?: string;
  // "menu" for a list of MenuItems; the name screen readers give the panel.
  role?: 'menu' | 'dialog';
  label?: string;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const [place, setPlace] = useState<{ top: number; left: number } | null>(null);
  useLayoutEffect(() => {
    if (!open) return;
    const update = () => {
      const r = anchor.current?.getBoundingClientRect();
      if (!r) return;
      const x = align === 'start' ? r.left : align === 'center' ? r.left + r.width / 2 - width / 2 : r.right - width;
      setPlace({ top: r.bottom + 8, left: Math.min(window.innerWidth - width - 8, Math.max(8, x)) });
    };
    update();
    window.addEventListener('resize', update);
    window.addEventListener('scroll', update, true);
    return () => {
      window.removeEventListener('resize', update);
      window.removeEventListener('scroll', update, true);
    };
  }, [open, anchor, width, align]);
  useEffect(() => {
    if (!open) return;
    const outside = (e: MouseEvent) => {
      if (!panel.current?.contains(e.target as Node) && !anchor.current?.contains(e.target as Node)) onClose();
    };
    const escape = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    document.addEventListener('mousedown', outside);
    document.addEventListener('keydown', escape);
    return () => {
      document.removeEventListener('mousedown', outside);
      document.removeEventListener('keydown', escape);
    };
  }, [open, onClose, anchor]);
  const placed = !!place;
  useEffect(() => {
    if (!open || !placed) return;
    const node = panel.current;
    (node?.querySelector<HTMLElement>('[data-autofocus]') ?? node?.querySelector<HTMLElement>(ITEMS))?.focus({ preventScroll: true });
    return () => {
      // Focus goes back to the button unless the user has clicked something else meanwhile.
      const active = document.activeElement;
      if (!active || active === document.body || node?.contains(active)) anchor.current?.focus({ preventScroll: true });
    };
  }, [open, placed, anchor]);
  if (!open || !place) return null;
  return createPortal(
    <div
      ref={panel}
      role={role}
      aria-label={label}
      style={{ top: place.top, left: place.left, width }}
      onKeyDown={event => {
        if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
        const items = [...(panel.current?.querySelectorAll<HTMLElement>(ITEMS) ?? [])];
        if (!items.length) return;
        event.preventDefault();
        const at = items.indexOf(document.activeElement as HTMLElement);
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 : event.key === 'ArrowDown' ? (at + 1) % items.length : (at - 1 + items.length) % items.length;
        items[next].focus();
      }}
      className={cx('glass-strong fixed z-40 rounded-[24px] p-2 animate-[sheet-in_0.22s_cubic-bezier(0.2,0.9,0.25,1)]', className)}
    >
      {children}
    </div>,
    document.body,
  );
}

export function MenuItem({ children, onClick, danger, icon }: { children: ReactNode; onClick: () => void; danger?: boolean; icon?: ReactNode }) {
  return (
    <button type="button" role="menuitem" onClick={onClick} className={cx('flex w-full items-center gap-2.5 rounded-[16px] px-3 py-2 text-left text-[13.5px] transition hover:bg-glass-strong focus-visible:bg-glass-strong', danger ? 'text-bad' : 'text-fg')}>
      {icon && <span className="text-fg-3">{icon}</span>}
      {children}
    </button>
  );
}
