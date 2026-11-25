// The dashboard shell: glass waves behind a floating navigation bar, three views and the sheets.
import { LogOut, Plus, Settings2 } from 'lucide-react';
import { useCallback, useEffect, useRef, useState } from 'react';
import type { Server } from '@admin/api';
import { Island } from '@admin/Island';
import { StoreProvider, useStore } from '@admin/store';
import { Clients } from '@admin/views/Clients';
import { NewClient } from '@admin/views/NewClient';
import { Network } from '@admin/views/Network';
import { Overview } from '@admin/views/Overview';
import { SettingsSheet } from '@admin/views/Settings';
import { AppBar } from '@shared/AppBar';
import { WaveBackground } from '@shared/WaveBackground';
import { Mark, useAppearance, type Appearance } from '@shared/appearance';
import { Button, Card, CardSkeleton, Field, IconButton, Loading, Segmented, Skeleton, UIProvider } from '@shared/glass';
import { useStoredState } from '@shared/hooks';
import { parseLoginLink } from './loginLink';
import { dashboardTransport, loginToken, signIn, signOut } from './session';

const VIEWS = ['overview', 'clients', 'network'] as const;
type View = (typeof VIEWS)[number];

export function App() {
  const appearance = useAppearance();
  const [server, setServer] = useState<Server | null>(null);
  const [phase, setPhase] = useState<'loading' | 'signin' | 'ready'>('loading');
  const [message, setMessage] = useState('');

  // Only the newest sign-in may set the page, if a second one starts while the first runs. A token
  // pasted into the sign-in card is used instead of the one in the address.
  const attempt = useRef(0);
  const start = useCallback((token?: string | null) => {
    const mine = ++attempt.current;
    setPhase('loading');
    signIn(token).then(result => {
      if (mine !== attempt.current) return;
      if (result.server) {
        setServer(result.server);
        setPhase('ready');
      } else {
        setMessage(result.error);
        setPhase('signin');
      }
    });
  }, []);
  useEffect(() => {
    start();
    // Opening the login link again in this tab changes only the part after #, which does not
    // reload the page: sign in with the new token here.
    const again = () => {
      if (loginToken()) start();
    };
    window.addEventListener('hashchange', again);
    return () => window.removeEventListener('hashchange', again);
  }, [start]);

  return (
    <UIProvider>
      <WaveBackground />
      <div className="relative z-10 min-h-screen">
        {phase === 'loading' && (
          <div className="grid min-h-screen place-items-center px-4">
            <Loading label="Connecting to the server" className="grid w-full max-w-md justify-items-center gap-4">
              <Mark className="size-12" />
              <CardSkeleton lines={2} className="w-full" />
              <Skeleton className="h-3 w-40" />
              <p className="text-[13px] text-fg-3" aria-hidden="true">
                Connecting to the server…
              </p>
            </Loading>
          </div>
        )}
        {phase === 'signin' && <SignIn message={message} onToken={start} />}
        {phase === 'ready' && server && (
          <StoreProvider
            transport={dashboardTransport}
            initial={server}
            onSignedOut={text => {
              setMessage(text);
              setPhase('signin');
            }}
          >
            <Shell appearance={appearance} onSignOut={() => signOut().then(() => (setMessage('Signed out.'), setPhase('signin')))} />
          </StoreProvider>
        )}
      </div>
    </UIProvider>
  );
}

function SignIn({ message, onToken }: { message: string; onToken: (token: string) => void }) {
  const [text, setText] = useState('');
  const [problem, setProblem] = useState('');
  const submit = () => {
    const token = parseLoginLink(text);
    if (!token) {
      setProblem('That is not a login link. It looks like https://…/#t=… and is printed by the command above.');
      return;
    }
    setProblem('');
    setText('');
    onToken(token);
  };
  return (
    <div className="grid min-h-screen place-items-center px-4">
      <Card className="w-full max-w-md text-center">
        <Mark className="mx-auto mb-5 size-12" />
        <h1 className="text-[24px]">Ghostlink server</h1>
        <p className="mt-2 text-[13.5px] text-fg-2">
          Open the login link printed by <code className="mono rounded-md bg-field px-1.5 py-0.5 text-[12.5px]">sudo ghostlink-server dashboard</code> on the server.
        </p>
        {message && <p className="mt-4 text-[13px] text-bad">{message}</p>}
        {/* The link may have lost its #t= on the way (a chat app, a redirect): paste it here. */}
        <form
          className="mt-5 grid gap-3 text-left"
          onSubmit={event => {
            event.preventDefault();
            submit();
          }}
        >
          <Field label="Or paste the link here" hint="the whole link, or just the part after #t=">
            <input className="field mono" type="password" autoComplete="off" spellCheck={false} autoFocus value={text} onChange={e => setText(e.target.value)} placeholder="https://…/#t=…" />
          </Field>
          {problem && <p className="text-[12.5px] text-bad">{problem}</p>}
          <Button type="submit" variant="cta" disabled={!text.trim()} className="justify-self-end">
            Sign in
          </Button>
        </form>
      </Card>
    </div>
  );
}

function Shell({ appearance, onSignOut }: { appearance: Appearance; onSignOut: () => void }) {
  const { server } = useStore();
  const [view, setView] = useStoredState<View>('ghostlink-view', 'overview', VIEWS);
  const [newClient, setNewClient] = useState(false);
  const [settings, setSettings] = useState(false);

  return (
    <>
      <AppBar
        appearance={appearance}
        subtitle={
          <div className="mono truncate text-[11.5px] text-fg-3" title={`${server.name} · version ${server.version}`}>
            {server.hostname} · v{server.version}
          </div>
        }
        switcher={
          <Segmented
            label="View"
            value={view}
            onChange={setView}
            className="shrink-0"
            options={[
              { value: 'overview', label: 'Overview' },
              { value: 'clients', label: 'Clients' },
              { value: 'network', label: 'Network' },
            ]}
          />
        }
        status={<Island />}
        actions={
          <>
            <IconButton label="Server settings" onClick={() => setSettings(true)}>
              <Settings2 size={17} />
            </IconButton>
            <Button variant="cta" icon={<Plus size={16} strokeWidth={2.4} />} onClick={() => setNewClient(true)} className="ml-1">
              New client
            </Button>
          </>
        }
      />
      <main className="mx-auto max-w-[2400px] px-3 pt-5 pb-24 sm:px-5 2xl:px-8">
        {view === 'overview' && <Overview onNewClient={() => setNewClient(true)} />}
        {view === 'clients' && <Clients onNewClient={() => setNewClient(true)} />}
        {view === 'network' && <Network />}
      </main>
      <NewClient open={newClient} onClose={() => setNewClient(false)} />
      <SettingsSheet
        open={settings}
        onClose={() => setSettings(false)}
        footer={
          <Button variant="ghost" icon={<LogOut size={15} />} onClick={onSignOut}>
            Sign out
          </Button>
        }
      />
    </>
  );
}
