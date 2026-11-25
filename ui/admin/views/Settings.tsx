// Server settings: name, the address in new keys, the REALITY site, the default speed limit and
// monitoring retention.
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Button, Check, Field, Inner, Note, Sheet, useUI } from '@shared/glass';
import { useBusy, usePoll } from '@shared/hooks';
import { defaultSpeedChange, defaultSpeedText } from '../limits';
import { lookupState } from '../provider';
import { useApi, useStore } from '../store';

export function SettingsSheet({ open, onClose, footer }: { open: boolean; onClose: () => void; footer?: ReactNode }) {
  const { server, reloadServer } = useStore();
  const { api } = useApi();
  const { toast, confirm } = useUI();
  const [name, setName] = useState(server.name);
  const [hostname, setHostname] = useState(server.hostname);
  const [reality, setReality] = useState(server.vless_handshake);
  const [sites, setSites] = useState(server.site_stats);
  const [provider, setProvider] = useState(server.asn?.enabled ?? false);
  const [historyDays, setHistoryDays] = useState(String(server.history_days));
  const [siteDays, setSiteDays] = useState(String(server.site_days));
  const [historyMB, setHistoryMB] = useState(String(server.history_max_mb));
  const [speedDefault, setSpeedDefault] = useState(defaultSpeedText(server.default_speed_limit_bps));
  const [error, setError] = useState('');
  const speedField = useRef<HTMLInputElement>(null);
  // One save at a time (a double click would send it twice), and one provider download or key
  // replacement at a time.
  const { busy, run } = useBusy();
  const { busy: working, run: work } = useBusy();
  // The list download takes a few seconds: while the server says it is updating, ask again so the
  // sheet shows when it is done instead of the state it had when it was opened.
  const updating = !!server.asn?.updating;
  const wasUpdating = useRef(false);
  usePoll(async () => {
    if (open && updating) await reloadServer();
  }, 2000, [open, updating]);
  useEffect(() => {
    if (wasUpdating.current && !updating && server.asn) {
      if (server.asn.error) toast(`The provider list could not be updated: ${server.asn.error}`, 'bad');
      else toast('The provider list is up to date.');
    }
    wasUpdating.current = updating;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [updating]);

  useEffect(() => {
    if (!open) return;
    setName(server.name);
    setHostname(server.hostname);
    setReality(server.vless_handshake);
    setSites(server.site_stats);
    setProvider(server.asn?.enabled ?? false);
    setHistoryDays(String(server.history_days));
    setSiteDays(String(server.site_days));
    setHistoryMB(String(server.history_max_mb));
    setSpeedDefault(defaultSpeedText(server.default_speed_limit_bps));
    setError('');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const save = () => run(saveNow);
  const saveNow = async () => {
    const patch: Record<string, unknown> = {};
    if (name.trim() !== server.name) patch.name = name.trim();
    if (hostname.trim() !== server.hostname) patch.hostname = hostname.trim();
    if (reality.trim() !== server.vless_handshake) patch.vless_handshake = reality.trim();
    if (sites !== server.site_stats) patch.site_stats = sites;
    if (server.asn && provider !== server.asn.enabled) patch.asn_lookup = provider;
    for (const [key, value, current, label] of [
      ['history_days', historyDays, server.history_days, 'History'],
      ['site_days', siteDays, server.site_days, 'Sites'],
      ['history_max_mb', historyMB, server.history_max_mb, 'Size limit'],
    ] as const) {
      const n = Number(value);
      if (!n || n === current) continue;
      // Whole numbers only: the desktop app's service refuses a fraction before the server sees it.
      if (!Number.isInteger(n) || n < 1) {
        setError(`${label} must be a whole number above zero.`);
        return;
      }
      patch[key] = n;
    }
    // A number field reports half-typed text such as "2e" as empty, which would remove the limit.
    const bps = speedField.current?.validity.badInput ? new Error('The default speed limit is a number of MB/s, or empty for none.') : defaultSpeedChange(speedDefault, server.default_speed_limit_bps ?? 0);
    if (bps instanceof Error) {
      setError(bps.message);
      return;
    }
    if (bps !== undefined) patch.default_speed_limit_bps = bps;
    if (!Object.keys(patch).length) {
      onClose();
      return;
    }
    if (patch.site_stats === false && !(await confirm({ title: 'Turn top services off?', text: 'This deletes every recorded site and service at once.', action: 'Turn off and delete', danger: true }))) return;
    try {
      await api('PATCH', '/server', patch);
      await reloadServer();
      toast(patch.hostname ? 'Saved. New keys use the new address; keys already shared keep the old one.' : 'Settings saved.');
      onClose();
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const updateList = () =>
    work(async () => {
      try {
        await api('POST', '/asn/update');
        await reloadServer();
        toast('Downloading the provider list…', 'info');
      } catch (e) {
        toast((e as Error).message, 'bad');
      }
    });
  const rotatePSK = () =>
    work(async () => {
      if (!(await confirm({ title: 'Replace the IPsec group key?', text: 'Every device that signs in with the shared group key is disconnected, and its profile must be installed again. Certificates and IKEv2 are not affected.', action: 'Replace the key', danger: true }))) return;
      try {
        await api('PATCH', '/server', { rotate_ipsec_psk: true });
        toast('The IPsec group key is replaced. Download the profiles of the clients that use it again.');
      } catch (e) {
        toast((e as Error).message, 'bad');
      }
    });

  return (
    <Sheet
      open={open}
      onClose={onClose}
      size="medium"
      title="Server settings"
      subtitle={`Ghostlink server ${server.version}`}
      footer={
        <>
          <div className="mr-auto">{footer}</div>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="cta" disabled={busy} onClick={save}>
            {busy ? 'Saving…' : 'Save'}
          </Button>
        </>
      }
    >
      <div className="grid gap-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Server name">
            <input className="field" data-autofocus maxLength={64} value={name} onChange={e => setName(e.target.value)} />
          </Field>
          <Field label="Address in new keys" hint="public IP or DNS name">
            <input className="field mono" spellCheck={false} value={hostname} onChange={e => setHostname(e.target.value)} />
          </Field>
        </div>
        <Field label="REALITY site" hint="a public TLS 1.3 site that VLESS imitates">
          <input className="field mono" spellCheck={false} value={reality} onChange={e => setReality(e.target.value)} />
        </Field>
        <div className="grid gap-2">
          <Field label="Default speed limit" hint="MB/s for every client without its own; empty for none">
            <input ref={speedField} className="field num w-40" type="number" min="0" max="1250" step="any" inputMode="decimal" placeholder="No limit" value={speedDefault} onChange={e => setSpeedDefault(e.target.value)} />
          </Field>
          <Note>One cap per client, download and upload alike, across all its devices. A client can have its own limit, or none, in its settings. Changes apply at once, without disconnecting anyone.</Note>
        </div>
        <Inner className="grid gap-3">
          <h3 className="text-[14px]">Monitoring</h3>
          <Check checked={sites} onChange={setSites}>
            Record top sites and services per client
          </Check>
          <Note>Off by default. When on, the server keeps per-client totals per service (for example "YouTube 4 GB"), never a list of pages, and deletes them after the retention below. Turning it off deletes every recorded site.</Note>
          {server.asn && (
            <>
              <Check checked={provider} onChange={setProvider}>
                Show the provider (AS and operator) of each client address
              </Check>
              <Note>
                Off by default. When on, the server downloads a public list of address ranges (about 11 MB) from {server.asn.source} and shows, beside each address a client connects from, who runs it (for example "AS64496 · Example Telecom"). Only the list is downloaded: nothing about your clients is sent to anyone (iptoasn.com sees this server's address, as any website does when you visit it). It is downloaded again every 30 days, and turning this off deletes it.
                {lookupState(server.asn, Date.now() / 1000) && <span className="mt-1 block text-fg-3">{lookupState(server.asn, Date.now() / 1000)}</span>}
              </Note>
              {server.asn.enabled && (
                <Button size="sm" variant="ghost" className="justify-self-start" disabled={updating || working} onClick={updateList}>
                  {updating ? 'Downloading the list…' : 'Update the list now'}
                </Button>
              )}
            </>
          )}
          <div className="grid grid-cols-3 gap-3">
            <Field label="History" hint="days">
              <input className="field num" type="number" min={7} max={365} value={historyDays} onChange={e => setHistoryDays(e.target.value)} />
            </Field>
            <Field label="Sites" hint="days">
              <input className="field num" type="number" min={1} max={90} value={siteDays} onChange={e => setSiteDays(e.target.value)} />
            </Field>
            <Field label="Size limit" hint="MB">
              <input className="field num" type="number" min={1} max={1024} value={historyMB} onChange={e => setHistoryMB(e.target.value)} />
            </Field>
          </div>
          <Note>Older records are deleted automatically, and the history file never grows past the size limit (oldest site totals go first, daily totals last).</Note>
        </Inner>
        {server.protocols.ipsec?.enabled && (
          <Inner className="grid gap-2">
            <h3 className="text-[14px]">IPsec group key</h3>
            <Note>The shared key of IPsec profiles that sign in with it instead of a certificate. Replace it if it may have leaked: every such profile must then be installed again.</Note>
            <Button size="sm" variant="ghost" className="justify-self-start" disabled={working} onClick={rotatePSK}>
              Replace the group key
            </Button>
          </Inner>
        )}
        <Inner className="grid gap-1.5 text-[13px]">
          <h3 className="text-[14px]">This dashboard</h3>
          <p className="text-fg-2">
            Port <span className="mono">{server.dashboard_port}</span>, {server.dashboard_public ? 'public HTTPS' : 'private (SSH tunnel only)'}. Change it on the server with <span className="mono">sudo ghostlink-server dashboard --port N</span>
            {server.dashboard_public ? ', or go back to tunnel-only with --private' : ''}.
          </p>
          <p className="text-fg-2">
            Management API on TCP <span className="mono">{server.api_port}</span>.
          </p>
        </Inner>
        {error && <Note tone="bad">{error}</Note>}
      </div>
    </Sheet>
  );
}
