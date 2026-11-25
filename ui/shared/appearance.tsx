// Appearance shared by both apps: the brand mark, the three finishes and the wave motion.
import { Palette } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { IconButton, Segmented, cx } from './glass';
import { prefersReducedMotion, useStoredState } from './hooks';
import { Popover } from './Popover';

export const FINISHES = ['obsidian', 'titanium', 'daylight'] as const;
export type Finish = (typeof FINISHES)[number];
export const MOTIONS = ['subtle', 'calm', 'still'] as const;
export type Motion = (typeof MOTIONS)[number];

export function Mark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 40 40" className={className} aria-hidden="true">
      <rect width="40" height="40" rx="12" className="fill-fg" />
      <path d="M12 29.5V19a8 8 0 0 1 16 0v10.5l-2.67-2-2.66 2-2.67-2-2.67 2-2.66-2z" fill="none" className="stroke-canvas" strokeWidth="2.6" strokeLinejoin="round" strokeLinecap="round" />
      <circle cx="17" cy="19.5" r="1.6" className="fill-canvas" />
      <circle cx="23" cy="19.5" r="1.6" className="fill-canvas" />
    </svg>
  );
}

export function useAppearance() {
  const [finish, setFinish] = useStoredState<Finish>('ghostlink-finish', 'obsidian', FINISHES);
  const [motion, setMotion] = useStoredState<Motion>('ghostlink-motion', prefersReducedMotion() ? 'still' : 'subtle', MOTIONS);
  useEffect(() => {
    document.documentElement.dataset.finish = finish;
    document.documentElement.dataset.motion = motion;
  }, [finish, motion]);
  return { finish, setFinish, motion, setMotion };
}
export type Appearance = ReturnType<typeof useAppearance>;

const swatch: Record<Finish, string> = {
  obsidian: 'bg-[#090a0d]',
  titanium: 'bg-[#2a2e37]',
  daylight: 'bg-[#f2f4f8]',
};

export function FinishPicker({ value, onChange }: { value: Finish; onChange: (f: Finish) => void }) {
  return (
    <div role="radiogroup" aria-label="Finish" className="inset-glass flex items-center gap-1 rounded-full p-1">
      {FINISHES.map(f => (
        <button
          key={f}
          type="button"
          role="radio"
          aria-checked={value === f}
          data-autofocus={value === f ? '' : undefined}
          title={f[0].toUpperCase() + f.slice(1)}
          onClick={() => onChange(f)}
          className={cx('size-6 rounded-full border transition', swatch[f], value === f ? 'border-fg ring-2 ring-fg/25' : 'border-line hover:scale-110')}
        />
      ))}
    </div>
  );
}

// The finish and the background motion, behind one button of the navigation bar: two controls that
// are used once in a while do not need room in a bar that must stay on one line.
export function AppearanceMenu({ appearance }: { appearance: Appearance }) {
  const [open, setOpen] = useState(false);
  const anchor = useRef<HTMLButtonElement>(null);
  return (
    <>
      <IconButton ref={anchor} label="Appearance: finish and background motion" aria-expanded={open} aria-haspopup="dialog" onClick={() => setOpen(!open)}>
        <Palette size={17} />
      </IconButton>
      <Popover anchor={anchor} open={open} onClose={() => setOpen(false)} width={288}>
        <div role="dialog" aria-label="Appearance" className="grid gap-3.5 p-2.5">
          <div className="flex items-center justify-between gap-3">
            <span className="text-[13px] text-fg-2">Finish</span>
            <FinishPicker value={appearance.finish} onChange={appearance.setFinish} />
          </div>
          <div className="grid gap-1.5">
            <span className="text-[13px] text-fg-2">Background motion</span>
            <Segmented
              size="sm"
              label="Background motion"
              value={appearance.motion}
              onChange={appearance.setMotion}
              options={[
                { value: 'subtle', label: 'Subtle' },
                { value: 'calm', label: 'Calm' },
                { value: 'still', label: 'Still' },
              ]}
            />
          </div>
        </div>
      </Popover>
    </>
  );
}
