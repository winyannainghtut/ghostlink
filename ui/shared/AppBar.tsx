// The navigation bar of both apps: the brand on the left, the section switcher and the status
// capsule in the middle, the actions on the right. It stays on one line from 1024 px up, also at the
// desktop app's default 1060 px: the middle takes what the sides leave and its capsule shrinks (its
// profile name is cut short) before anything wraps, and the live speeds appear only when the middle
// is wide enough for them (a container query on the middle).
import type { ReactNode } from 'react';
import { AppearanceMenu, Mark, type Appearance } from './appearance';

export function AppBar({ subtitle, switcher, status, appearance, actions }: {
  // The line under the name: the service or the server this bar is about.
  subtitle: ReactNode;
  switcher: ReactNode;
  status: ReactNode;
  appearance: Appearance;
  // The buttons after the appearance menu: full screen, and the main action of the view.
  actions: ReactNode;
}) {
  return (
    <header className="sticky top-0 z-30 px-3 pt-3 sm:px-5 sm:pt-4 2xl:px-8">
      <nav className="glass mx-auto flex max-w-[2400px] flex-wrap items-center gap-x-4 gap-y-3 rounded-[28px] px-4 py-3 lg:flex-nowrap">
        <div className="flex min-w-0 items-center gap-3 lg:shrink">
          <Mark className="size-9 shrink-0" />
          <div className="min-w-0 leading-tight">
            <div className="text-[15px] font-semibold tracking-[-0.02em]">GHOSTLINK</div>
            {subtitle}
          </div>
        </div>
        <div className="@container order-3 flex w-full flex-wrap items-center justify-center gap-3 lg:order-none lg:w-auto lg:min-w-0 lg:flex-1 lg:flex-nowrap">
          {switcher}
          {status}
        </div>
        <div className="ml-auto flex shrink-0 items-center justify-end gap-1.5">
          <AppearanceMenu appearance={appearance} />
          {actions}
        </div>
      </nav>
    </header>
  );
}
