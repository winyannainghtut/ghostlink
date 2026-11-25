// Split tunneling for all servers, or for one server (which can follow the setting for all).
import { useEffect, useId, useState } from 'react';
import { ListNote } from '@admin/RulesEditor';
import { checkAddresses, checkApps, checkSites } from '@admin/ruleCheck';
import { lines } from '@shared/format';
import { Button, Check, Field, Loading, Note, Segmented, Sheet, Skeleton, useUI } from '@shared/glass';
import { useBusy } from '@shared/hooks';
import { isActive, request, type Profile, type RoutingPolicy, type SplitMode } from '../ipc';
import { useService } from '../service';

export function splitSummary(policy: RoutingPolicy | null): string {
  const lan = policy?.lan_direct && policy.mode !== 'only' ? ' Local network stays direct.' : '';
  if (!policy || policy.mode === 'full') return 'Everything uses the VPN.' + lan;
  const count = policy.domains.length + policy.ip_ranges.length + policy.apps.length;
  const items = `${count} item${count === 1 ? '' : 's'}`;
  return (policy.mode === 'bypass' ? `${items} skip the VPN.` : `Only ${items} use the VPN.`) + lan;
}

// An example of an app for the field: the apps of this system are not the other one's.
const appExample = typeof navigator !== 'undefined' && /windows/i.test(navigator.userAgent) ? 'chrome.exe' : 'firefox';

// profile: undefined = closed, null = all servers, a profile = that server.
export function SplitSheet({ profile, onClose, onSaved }: { profile: Profile | null | undefined; onClose: () => void; onSaved?: () => void }) {
  const { info } = useService();
  const { toast } = useUI();
  const uid = useId();
  const [follow, setFollow] = useState(false);
  const [mode, setMode] = useState<SplitMode>('full');
  const [sites, setSites] = useState('');
  const [ips, setIps] = useState('');
  const [apps, setApps] = useState('');
  const [lan, setLan] = useState(false);
  const [error, setError] = useState('');
  // The form shows the policy as the service has it. Until that answer is in (or if it never comes)
  // the fields hold defaults, and saving them would replace the policy with "everything uses the
  // VPN": Save waits for the answer.
  const [loaded, setLoaded] = useState<'loading' | 'ready' | 'failed'>('loading');
  const { busy, run } = useBusy();
  const open = profile !== undefined;

  useEffect(() => {
    if (!open) return;
    let current = true;
    setError('');
    setLoaded('loading');
    request<{ policy: RoutingPolicy | null; follows_global?: boolean }>(profile ? { type: 'routing_policy_get', profile: profile.name } : { type: 'routing_policy_get' })
      .then(data => {
        if (!current) return;
        const p = data.policy ?? { mode: 'full', domains: [], ip_ranges: [], apps: [], lan_direct: false };
        setFollow(!!profile && !!data.follows_global);
        setMode(p.mode);
        setSites(p.domains.join('\n'));
        setIps(p.ip_ranges.join('\n'));
        setApps(p.apps.join('\n'));
        setLan(p.lan_direct);
        setLoaded('ready');
      })
      .catch(e => {
        if (!current) return;
        setError(`The current settings could not be read: ${e.message}`);
        setLoaded('failed');
      });
    return () => {
      current = false;
    };
  }, [open, profile]);

  // The first list gets the focus once the form is there (the sheet opens before its answer arrives).
  useEffect(() => {
    if (loaded === 'ready' && mode !== 'full') document.querySelector<HTMLElement>('dialog[open] textarea[data-autofocus]')?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [loaded]);
  const editing = !(profile && follow);
  // The service checks every list in every mode, also the ones the current mode does not show. The
  // same checks run here as you type, and the first mistake is named beside its list.
  const siteCheck = checkSites(lines(sites), 'split'), ipCheck = checkAddresses(lines(ips)), appCheck = checkApps(lines(apps));
  const mistake = siteCheck.problem || ipCheck.problem || appCheck.problem;
  const nothingListed = mode === 'only' && siteCheck.count + ipCheck.count + appCheck.count === 0;
  const blocked = editing && (!!mistake || nothingListed);

  const save = () =>
    run(async () => {
      const policy = profile && follow ? null : { mode, domains: lines(sites), ip_ranges: lines(ips), apps: lines(apps), lan_direct: lan };
      try {
        await request({ type: 'routing_policy_set', policy, ...(profile ? { profile: profile.name } : {}) });
        toast(isActive(info?.connection.status) ? 'Split tunneling saved. Disconnect and connect again to apply it.' : 'Split tunneling saved. It applies the next time you connect.');
        onSaved?.();
        onClose();
      } catch (e) {
        setError((e as Error).message);
      }
    });
  return (
    <Sheet
      open={open}
      onClose={onClose}
      size="medium"
      title="Split tunneling"
      subtitle={profile ? `For ${profile.display_name || profile.name}` : 'For all servers'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="cta" disabled={busy || loaded !== 'ready' || blocked} onClick={save}>
            {busy ? 'Saving…' : 'Save'}
          </Button>
        </>
      }
    >
      {loaded === 'loading' ? (
        <Loading label="Reading the current settings" className="grid gap-4">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-[120px] w-full rounded-[18px]" />
        </Loading>
      ) : (
        <div className="grid gap-5">
          {loaded === 'ready' && profile && (
            <Check checked={follow} onChange={setFollow}>
              Use the settings for all servers
            </Check>
          )}
          {loaded === 'ready' && editing && (
            <>
              <Segmented
                label="Mode"
                value={mode}
                onChange={setMode}
                options={[
                  { value: 'full', label: 'Everything uses the VPN' },
                  { value: 'bypass', label: 'Listed items skip it' },
                  { value: 'only', label: 'Only listed items use it' },
                ]}
              />
              {mode !== 'full' && (
                <div className="grid gap-4 md:grid-cols-3">
                  <div className="grid content-start gap-1.5">
                    <Field label="Sites" hint="subdomains included">
                      <textarea className="field min-h-[120px]" spellCheck={false} value={sites} onChange={e => setSites(e.target.value)} placeholder="example.com" data-autofocus aria-invalid={!!siteCheck.problem} aria-describedby={`${uid}-sites`} />
                    </Field>
                    <ListNote id={`${uid}-sites`} checked={siteCheck} noun="site" />
                  </div>
                  <div className="grid content-start gap-1.5">
                    <Field label="IP ranges">
                      <textarea className="field min-h-[120px]" spellCheck={false} value={ips} onChange={e => setIps(e.target.value)} placeholder="192.168.1.0/24" aria-invalid={!!ipCheck.problem} aria-describedby={`${uid}-ips`} />
                    </Field>
                    <ListNote id={`${uid}-ips`} checked={ipCheck} noun="range" />
                  </div>
                  <div className="grid content-start gap-1.5">
                    <Field label="Apps" hint="executable name or path">
                      <textarea className="field min-h-[120px]" spellCheck={false} value={apps} onChange={e => setApps(e.target.value)} placeholder={appExample} aria-invalid={!!appCheck.problem} aria-describedby={`${uid}-apps`} />
                    </Field>
                    <ListNote id={`${uid}-apps`} checked={appCheck} noun="app" />
                  </div>
                </div>
              )}
              {mode === 'only' && nothingListed && <Note tone="warn">Only listed items use the VPN, so list at least one site, IP range or app.</Note>}
              {mode === 'full' && mistake && (
                <Note tone="bad">
                  A list kept for the other modes has a mistake ({mistake.line ? `line ${mistake.line}: ` : ''}
                  {mistake.reason}) Choose “Listed items skip it” or “Only listed items use it” to see and correct it.
                </Note>
              )}
              {mode !== 'only' && (
                <Check checked={lan} onChange={setLan}>
                  Keep local network devices off the VPN
                </Check>
              )}
              <Note>Sites are matched by name, so a few apps that hide it can still use the VPN. Changes apply the next time you connect.</Note>
            </>
          )}
          {error && <Note tone="bad">{error}</Note>}
        </div>
      )}
    </Sheet>
  );
}
