// Servers I run: Ghostlink servers this device manages with their management keys (sealed in
// the local service). Opening one shows the same views as the server's own dashboard.
import { ArrowLeft, BookOpen, Eye, EyeOff, KeyRound, Plus, RotateCw, Server as ServerIcon, Settings2, Trash2 } from 'lucide-react';
import { useEffect, useState } from 'react';
import type { Server, Transport } from '@admin/api';
import { Island } from '@admin/Island';
import { StoreProvider, useStore } from '@admin/store';
import { Clients } from '@admin/views/Clients';
import { NewClient } from '@admin/views/NewClient';
import { Network } from '@admin/views/Network';
import { Overview } from '@admin/views/Overview';
import { SettingsSheet } from '@admin/views/Settings';
import { dateTime } from '@shared/format';
import { Button, Card, CardSkeleton, Empty, Field, IconButton, Loading, Note, Sheet, Tabs, useUI } from '@shared/glass';
import { useBusy, useStoredState } from '@shared/hooks';
import { request, slug, uniqueName, type ManagedServer } from '../ipc';
import { serverTransport } from '../serverTransport';
import { useService } from '../service';

type Seen = Record<string, { name: string; version: string }>;

// Forgetting a server only removes its management key from this device: the service needs no
// answer from the server for it. So it is offered wherever the server is listed, also for one that
// does not answer (switched off, reinstalled with a new key, its address gone), which no view of
// the server itself can be opened for. Resolves to whether it was forgotten.
// What a server is called in the lists and on its pages: the name its owner gave it on this device,
// else its ID. (Not the name the server calls itself: that is "Ghostlink server" unless it was named
// at install, so servers would look alike.)
const titleOf = (s: { name: string; label?: string }) => s.label || s.name;

function useForgetServer() {
  const { confirm, toast } = useUI();
  return async (server: { name: string }, title: string): Promise<boolean> => {
    if (!(await confirm({ title: `Forget ${title}?`, text: 'Ghostlink removes its management key from this device. The server, its clients and their keys keep working. You need the key to add it again.', action: 'Forget server', danger: true }))) return false;
    try {
      await request({ type: 'managed_server_remove', name: server.name });
      toast(`${title} forgotten on this device.`);
      return true;
    } catch (e) {
      toast((e as Error).message, 'bad');
      return false;
    }
  };
}

// addServerSignal counts presses of the navigation bar's Add server; onAddServerShown clears it
// once the sheet is open, so coming back to this view later does not open it again. onWorkspace
// says whether a server's workspace is open: the bar drops its own main action then.
export function ServersIRun({ addServerSignal, onAddServerShown, onWorkspace }: { addServerSignal: number; onAddServerShown: () => void; onWorkspace: (open: boolean) => void }) {
  const { toast } = useUI();
  const [servers, setServers] = useState<ManagedServer[] | null>(null);
  // The service keeps each server's name and version from the day it was added; the servers
  // themselves tell the current ones (after a rename or an upgrade).
  const [seen, setSeen] = useState<Seen>({});
  // Servers that did not answer, with why: the card says so, and offers to forget the server.
  const [silent, setSilent] = useState<Record<string, string>>({});
  const [adding, setAdding] = useState(false);
  const [help, setHelp] = useState(false);
  const [open, setOpen] = useState<ManagedServer | null>(null);
  const [renewing, setRenewing] = useState<ManagedServer | null>(null);
  const forget = useForgetServer();
  const inWorkspace = open !== null;
  useEffect(() => {
    onWorkspace(inWorkspace);
    return () => onWorkspace(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inWorkspace]);
  const load = () =>
    request<{ servers: ManagedServer[] }>({ type: 'managed_server_list' })
      .then(r => {
        setServers(r.servers);
        // A server forgotten or added since leaves the lists of what was seen and not answered.
        const names = new Set(r.servers.map(s => s.name));
        setSeen(all => Object.fromEntries(Object.entries(all).filter(([name]) => names.has(name))));
        setSilent(all => Object.fromEntries(Object.entries(all).filter(([name]) => names.has(name))));
        for (const s of r.servers) {
          request<Server>({ type: 'server_call', server: s.name, op: { op: 'info' } })
            .then(info => {
              setSeen(all => ({ ...all, [s.name]: { name: info.name, version: info.version } }));
              setSilent(all => Object.fromEntries(Object.entries(all).filter(([name]) => name !== s.name)));
            })
            .catch(e => setSilent(all => ({ ...all, [s.name]: (e as Error).message })));
        }
      })
      .catch(e => (toast(e.message, 'bad'), setServers([])));
  useEffect(() => void load(), []);
  useEffect(() => {
    if (!addServerSignal) return;
    setOpen(null);
    setAdding(true);
    onAddServerShown();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [addServerSignal]);

  const current = (s: ManagedServer): ManagedServer => ({ ...s, server_name: seen[s.name]?.name || s.server_name, version: seen[s.name]?.version || s.version });
  if (open) return <Workspace server={current(open)} onBack={() => (setOpen(null), load())} />;
  return (
    <div className="grid gap-4">
      <div className="flex flex-wrap items-end justify-between gap-3 px-1">
        <div>
          <h1 className="text-[26px] tracking-[-0.03em]">Servers I run</h1>
          <p className="text-[13.5px] text-fg-2">Create clients and keys, set rules and watch usage on your Ghostlink servers.</p>
        </div>
        <Button icon={<BookOpen size={15} />} onClick={() => setHelp(true)}>
          Set up a server
        </Button>
      </div>
      {servers === null ? (
        <Loading label="Loading your servers" className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3 min-[112.5rem]:grid-cols-4 min-[143.75rem]:grid-cols-5">
          {[0, 1, 2].map(i => (
            <CardSkeleton key={i} lines={1} className="min-h-[8.75rem]" />
          ))}
        </Loading>
      ) : !servers.length ? (
        <Card>
          <Empty icon={<ServerIcon size={22} />} title="Run your own server">
            <p>Install Ghostlink server on a Linux machine with one command, then paste the management key it prints.</p>
            <div className="mt-4 flex justify-center gap-2">
              <Button onClick={() => setHelp(true)}>Show me how</Button>
              <Button variant="cta" icon={<Plus size={15} />} onClick={() => setAdding(true)}>
                Add server
              </Button>
            </div>
          </Empty>
        </Card>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3 min-[112.5rem]:grid-cols-4 min-[143.75rem]:grid-cols-5">
          {servers.map(current).map(s => (
            <Card as="article" key={s.name} className="flex flex-col gap-4 sm:p-5!">
              <div className="flex items-start gap-3">
                <div className="grid size-11 shrink-0 place-items-center rounded-[16px] bg-glass-strong text-fg-2">
                  <ServerIcon size={18} />
                </div>
                <div className="min-w-0 flex-1">
                  <h3 className="truncate text-[16px]">{titleOf(s)}</h3>
                  <div className="mono truncate text-[12px] text-fg-3">{s.endpoint}</div>
                </div>
              </div>
              <div className="flex items-center justify-between gap-3 border-t border-line pt-3 text-[12.5px] text-fg-3">
                <span className="min-w-0">
                  {silent[s.name] !== undefined && (
                    <>
                      <span className="text-bad" title={silent[s.name]}>
                        Not answering
                      </span>
                      {' · '}
                    </>
                  )}
                  {s.version ? `Ghostlink server ${s.version}` : 'Ghostlink server'} · added {dateTime(s.added_unix)}
                </span>
                <div className="flex shrink-0 items-center gap-1.5">
                  {silent[s.name] !== undefined && (
                    <Button size="sm" variant="ghost" icon={<KeyRound size={13} />} onClick={() => setRenewing(s)}>
                      Update key
                    </Button>
                  )}
                  <IconButton label={`Forget ${titleOf(s)}`} onClick={async () => (await forget(s, titleOf(s))) && load()}>
                    <Trash2 size={15} />
                  </IconButton>
                  <Button size="sm" variant="cta" onClick={() => setOpen(s)}>
                    Manage
                  </Button>
                </div>
              </div>
            </Card>
          ))}
        </div>
      )}
      <AddServerSheet open={adding} taken={(servers ?? []).map(s => s.name)} onClose={() => setAdding(false)} onAdded={load} />
      <UpdateKeySheet server={renewing} onClose={() => setRenewing(null)} onUpdated={load} />
      <SetupHelp open={help} onClose={() => setHelp(false)} />
    </div>
  );
}

const VIEWS = ['overview', 'clients', 'network'] as const;
type View = (typeof VIEWS)[number];

// Each server's views get their own transport, through their StoreProvider (useApi), so nothing
// started in one server's workspace reaches another server (code review 2026-11-09, F2).
function Workspace({ server, onBack }: { server: ManagedServer; onBack: () => void }) {
  const { refresh } = useService();
  const [open, setOpen] = useState<{ name: string; transport: Transport; info: Server } | null>(null);
  const [error, setError] = useState('');
  // Counts presses of Try again.
  const [attempt, setAttempt] = useState(0);
  const [renewing, setRenewing] = useState(false);
  const forget = useForgetServer();
  useEffect(() => {
    let current = true;
    const transport = serverTransport(server.name, refresh);
    setOpen(null);
    setError('');
    transport
      .call<Server>('GET', '/server')
      .then(info => current && setOpen({ name: server.name, transport, info }))
      .catch(e => current && setError(e.message));
    return () => {
      current = false;
    };
  }, [server.name, refresh, attempt]);
  if (error)
    return (
      <Card>
        <Empty icon={<ServerIcon size={22} />} title={`${titleOf(server)} did not answer`}>
          <p>{error}</p>
          <p className="mt-2 text-[12.5px] text-fg-3">If you replaced the server's management key (rotate-key) or reinstalled it, its key changed: update the key here and the server keeps its name and place on this device.</p>
          <div className="mt-4 flex flex-wrap justify-center gap-2">
            <Button icon={<ArrowLeft size={15} />} onClick={onBack}>
              All servers I run
            </Button>
            <Button icon={<RotateCw size={15} />} onClick={() => setAttempt(n => n + 1)}>
              Try again
            </Button>
            <Button variant="cta" icon={<KeyRound size={15} />} onClick={() => setRenewing(true)}>
              Update key
            </Button>
            <Button variant="danger" icon={<Trash2 size={14} />} onClick={async () => (await forget(server, titleOf(server))) && onBack()}>
              Forget this server
            </Button>
          </div>
        </Empty>
        <UpdateKeySheet server={renewing ? server : null} onClose={() => setRenewing(false)} onUpdated={() => setAttempt(n => n + 1)} />
      </Card>
    );
  if (!open || open.name !== server.name) return <WorkspaceLoading title={titleOf(server)} onBack={onBack} />;
  // Keyed by server: another server gets a new store, never this one's state.
  return (
    <StoreProvider key={open.name} transport={open.transport} initial={open.info} onSignedOut={() => {}}>
      <WorkspaceBody server={server} onBack={onBack} />
    </StoreProvider>
  );
}

// What shows while the server answers its first request: the header of the workspace, so the page
// does not jump, over placeholders of the Overview.
function WorkspaceLoading({ title, onBack }: { title: string; onBack: () => void }) {
  return (
    <div className="grid gap-4">
      <div className="flex items-center gap-3">
        <IconButton label="All servers I run" onClick={onBack}>
          <ArrowLeft size={18} />
        </IconButton>
        <div className="min-w-0">
          <h1 className="truncate text-[20px]">{title}</h1>
          <p className="text-[12px] text-fg-3">Connecting to the server…</p>
        </div>
      </div>
      <Loading label={`Connecting to ${title}`} className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {[0, 1, 2, 3].map(i => (
          <CardSkeleton key={i} lines={2} className="min-h-[9.75rem]" />
        ))}
      </Loading>
    </div>
  );
}

// The page header of one server. It is not a second navigation bar: no frosted bar, a single title
// line with the server's status (frosted, not the black capsule of the bar above) and its two
// actions, and the pages as underlined tabs under it. The app bar above keeps the switcher of the
// whole app and the only black capsule.
function WorkspaceBody({ server, onBack }: { server: ManagedServer; onBack: () => void }) {
  const { server: info } = useStore();
  const forgetServer = useForgetServer();
  const [view, setView] = useStoredState<View>('ghostlink-desktop-admin-view', 'overview', VIEWS);
  const [newClient, setNewClient] = useState(false);
  const [settings, setSettings] = useState(false);
  const forget = async () => {
    if (await forgetServer(server, titleOf(server))) onBack();
  };
  return (
    <div className="grid gap-4">
      <div className="@container grid gap-2">
        <div className="flex flex-wrap items-center gap-3">
          <IconButton label="All servers I run" onClick={onBack}>
            <ArrowLeft size={18} />
          </IconButton>
          <div className="min-w-0 flex-1 basis-56">
            <h1 className="truncate text-[20px]">{titleOf(server)}</h1>
            <div className="mono truncate text-[11.5px] text-fg-3">
              {info.name !== titleOf(server) ? `${info.name} · ` : ''}
              {info.hostname} · v{info.version}
            </div>
          </div>
          <Island tone="glass" />
          <IconButton label="Server settings" onClick={() => setSettings(true)}>
            <Settings2 size={17} />
          </IconButton>
          <Button variant="cta" icon={<Plus size={15} strokeWidth={2.4} />} onClick={() => setNewClient(true)}>
            New client
          </Button>
        </div>
        <Tabs
          label="Pages of this server"
          value={view}
          onChange={setView}
          options={[
            { value: 'overview', label: 'Overview' },
            { value: 'clients', label: 'Clients' },
            { value: 'network', label: 'Network' },
          ]}
        />
      </div>
      {view === 'overview' && <Overview onNewClient={() => setNewClient(true)} />}
      {view === 'clients' && <Clients onNewClient={() => setNewClient(true)} />}
      {view === 'network' && <Network />}
      <NewClient open={newClient} onClose={() => setNewClient(false)} />
      <SettingsSheet
        open={settings}
        onClose={() => setSettings(false)}
        footer={
          <Button variant="danger" icon={<Trash2 size={14} />} onClick={() => (setSettings(false), forget())}>
            Forget server
          </Button>
        }
      />
    </div>
  );
}

// A server that no longer answers with the key saved for it (its management key was replaced with
// rotate-key, or it was reinstalled): the new key replaces the old one, and the server keeps its
// ID, its name on this device and its place in the list.
function UpdateKeySheet({ server, onClose, onUpdated }: { server: ManagedServer | null; onClose: () => void; onUpdated: () => void }) {
  const { toast } = useUI();
  const [key, setKey] = useState('');
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState('');
  const { busy, run } = useBusy();
  useEffect(() => {
    setKey('');
    setReveal(false);
    setError('');
  }, [server?.name]);
  const update = () =>
    run(async () => {
      if (!server) return;
      setError('');
      try {
        // No label: replacing a key without one keeps the name given on this device.
        await request({ type: 'managed_server_add', name: server.name, management_key: key.trim(), replace: true });
        setKey('');
        toast(`The key of ${titleOf(server)} is updated.`);
        onUpdated();
        onClose();
      } catch (e) {
        setError((e as Error).message);
      }
    });
  return (
    <Sheet
      open={server !== null}
      onClose={onClose}
      title={server ? `Update the key of ${titleOf(server)}` : 'Update key'}
      subtitle="Paste the new management key printed by sudo ghostlink-server show-key."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="cta" disabled={busy || !key.trim()} onClick={update}>
            {busy ? 'Checking the server…' : 'Update key'}
          </Button>
        </>
      }
    >
      <form
        className="grid gap-4"
        onSubmit={e => {
          e.preventDefault();
          update();
        }}
      >
        <Field label="New management key">
          <div className="relative">
            <input className="field mono pr-12" type={reveal ? 'text' : 'password'} value={key} onChange={e => setKey(e.target.value)} placeholder='{"apiUrl":"https://…","certSha256":"…"}' autoComplete="off" spellCheck={false} data-autofocus />
            <IconButton label={reveal ? 'Hide key' : 'Show key'} className="absolute top-0.5 right-1" onClick={() => setReveal(!reveal)}>
              {reveal ? <EyeOff size={16} /> : <Eye size={16} />}
            </IconButton>
          </div>
        </Field>
        <Note>Ghostlink checks that the server answers with this key, then replaces the saved one. The server keeps the name you gave it on this device, and its clients and keys are not touched.</Note>
        {error && <Note tone="bad">{error}</Note>}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}

// The name typed here only becomes the server's ID on this device (letters, digits, - and _), so
// a name in another script, or one already used, still needs an ID of its own.
function AddServerSheet({ open, taken, onClose, onAdded }: { open: boolean; taken: string[]; onClose: () => void; onAdded: () => void }) {
  const { toast } = useUI();
  const [name, setName] = useState('');
  const [key, setKey] = useState('');
  const [reveal, setReveal] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  useEffect(() => {
    if (!open) setKey('');
    else {
      setName('');
      setError('');
      setReveal(false);
    }
  }, [open]);
  const add = async () => {
    setBusy(true);
    setError('');
    try {
      await request({ type: 'managed_server_add', name: uniqueName(slug(name.trim()).slice(0, 40), taken), label: name.trim(), management_key: key.trim() });
      setKey('');
      toast('Server added. Its management key is sealed in the local service.');
      onAdded();
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Sheet
      open={open}
      onClose={onClose}
      title="Add a server"
      subtitle="Paste the management key printed by sudo ghostlink-server show-key."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="cta" disabled={busy || !name.trim() || !key.trim()} onClick={add}>
            {busy ? 'Checking the server…' : 'Add server'}
          </Button>
        </>
      }
    >
      <form
        className="grid gap-4"
        onSubmit={e => {
          e.preventDefault();
          add();
        }}
      >
        <Field label="Name on this device">
          <input className="field" maxLength={64} value={name} onChange={e => setName(e.target.value)} placeholder="For example home" autoComplete="off" data-autofocus />
        </Field>
        <Field label="Management key">
          <div className="relative">
            <input className="field mono pr-12" type={reveal ? 'text' : 'password'} value={key} onChange={e => setKey(e.target.value)} placeholder='{"apiUrl":"https://…","certSha256":"…"}' autoComplete="off" spellCheck={false} />
            <IconButton label={reveal ? 'Hide key' : 'Show key'} className="absolute top-0.5 right-1" onClick={() => setReveal(!reveal)}>
              {reveal ? <EyeOff size={16} /> : <Eye size={16} />}
            </IconButton>
          </div>
        </Field>
        <Note>The key gives full control of the server. Ghostlink seals it in the local service and never shows it again.</Note>
        {error && <Note tone="bad">{error}</Note>}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}

function SetupHelp({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { copy } = useUI();
  const command = 'curl -fsSL https://raw.githubusercontent.com/winyannainghtut/GHOSTLINK/main/scripts/install-server.sh | sudo bash';
  return (
    <Sheet open={open} onClose={onClose} size="medium" title="Set up a Ghostlink server" subtitle="A Linux machine (Debian, Ubuntu, Fedora, RHEL and similar) reachable from the internet" footer={<Button variant="cta" onClick={onClose}>Done</Button>}>
      <ol className="grid list-none gap-4 p-0 text-[13.5px] text-fg-2">
        <li>
          <span className="text-fg">1. Install it with one command</span> on the server. It installs Docker if needed, verifies the package and starts the server:
          <button type="button" onClick={() => copy(command, 'Command copied.')} className="inset-glass mono mt-2 block w-full rounded-[18px] px-4 py-3 text-left text-[12px] break-all text-fg hover:bg-glass-strong" title="Copy">
            {command}
          </button>
          <span className="mt-1.5 block text-[12.5px] text-fg-3">
            Add <span className="mono">-s -- --hostname YOUR_PUBLIC_IP</span>, <span className="mono">--ipsec</span> for iPhone/iPad/Mac, or <span className="mono">--mesh</span> for a mesh of your Linux servers. Already running Outline there? Add <span className="mono">--migrate-outline</span> (try <span className="mono">--migrate-outline --check</span> first): every Outline key becomes a client and keeps working.
          </span>
        </li>
        <li>
          <span className="text-fg">2. Copy the management key</span> it prints. Show it again with <span className="mono">sudo ghostlink-server show-key</span>.
        </li>
        <li>
          <span className="text-fg">3. Add server here</span> and paste the key.
        </li>
        <li>
          <span className="text-fg">4. Open the ports</span> the installer lists in the server's firewall and your cloud security group.
          <span className="mt-1.5 block text-[12.5px] text-fg-3">A new server runs only Shadowsocks (Outline-compatible). To offer others, turn them on under Manage, then Network, and allow the port each one names.</span>
        </li>
      </ol>
    </Sheet>
  );
}
