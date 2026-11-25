// Traffic rules for everyone or for one client: blocked sites, addresses and ports, BitTorrent and
// outgoing mail. Saving replaces the rules.
import { useEffect, useId, useState } from 'react';
import { lines } from '@shared/format';
import { Button, Check, Field, Note, cx } from '@shared/glass';
import type { Rules } from './api';
import { checkAddresses, checkPorts, checkSites, describe, type Checked } from './ruleCheck';

// The line under a list: how many entries it has, or the first one that is not valid.
export function ListNote({ id, checked, noun, many }: { id: string; checked: Checked; noun: string; many?: string }) {
  const { ok, text } = describe(checked, noun, many);
  // The height is kept, so the count appearing does not push the form down.
  return (
    <p id={id} role={ok ? undefined : 'alert'} className={cx('min-h-[1.125rem] text-[12px] leading-[1.125rem]', ok ? 'text-fg-3' : 'text-bad')}>
      {text}
    </p>
  );
}

export function RulesEditor({ rules, onSave, saveLabel = 'Save rules' }: { rules: Rules; onSave: (rules: Rules) => Promise<void>; saveLabel?: string }) {
  const [sites, setSites] = useState(rules.blocked_domains.join('\n'));
  const [ips, setIps] = useState(rules.blocked_ips.join('\n'));
  const [ports, setPorts] = useState(rules.blocked_ports.join('\n'));
  const [torrent, setTorrent] = useState(rules.block_bittorrent);
  const [smtp, setSmtp] = useState(rules.block_smtp);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  // Two editors can be on the page at once (the rules for everyone and a client's): their notes
  // need ids of their own for aria-describedby.
  const uid = useId();
  // Follow the saved rules only when they change: a client's rules arrive as a new object with
  // every 15 s poll, which must not throw away what is being typed.
  const saved = JSON.stringify(rules);
  useEffect(() => {
    setSites(rules.blocked_domains.join('\n'));
    setIps(rules.blocked_ips.join('\n'));
    setPorts(rules.blocked_ports.join('\n'));
    setTorrent(rules.block_bittorrent);
    setSmtp(rules.block_smtp);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saved]);
  const siteCheck = checkSites(lines(sites), 'rules'), ipCheck = checkAddresses(lines(ips)), portCheck = checkPorts(lines(ports));
  const invalid = !!(siteCheck.problem || ipCheck.problem || portCheck.problem);
  return (
    <form
      className="grid grid-cols-1 gap-4"
      onSubmit={async event => {
        event.preventDefault();
        if (busy || invalid) return;
        setBusy(true);
        setError('');
        try {
          await onSave({ blocked_domains: lines(sites), blocked_ips: lines(ips), blocked_ports: lines(ports), block_bittorrent: torrent, block_smtp: smtp });
        } catch (e) {
          setError((e as Error).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      <div className="grid gap-4 md:grid-cols-3">
        <div className="grid content-start gap-1.5">
          <Field label="Blocked sites" hint="one per line, subdomains included">
            <textarea className="field min-h-[120px]" spellCheck={false} value={sites} onChange={e => setSites(e.target.value)} placeholder="example.com" aria-invalid={!!siteCheck.problem} aria-describedby={`${uid}-sites`} />
          </Field>
          <ListNote id={`${uid}-sites`} checked={siteCheck} noun="site" />
        </div>
        <div className="grid content-start gap-1.5">
          <Field label="Blocked addresses" hint="IP or CIDR">
            <textarea className="field min-h-[120px]" spellCheck={false} value={ips} onChange={e => setIps(e.target.value)} placeholder="203.0.113.0/24" aria-invalid={!!ipCheck.problem} aria-describedby={`${uid}-ips`} />
          </Field>
          <ListNote id={`${uid}-ips`} checked={ipCheck} noun="address" many="addresses" />
        </div>
        <div className="grid content-start gap-1.5">
          <Field label="Blocked ports" hint="25 or 6881-6889">
            <textarea className="field min-h-[120px]" spellCheck={false} value={ports} onChange={e => setPorts(e.target.value)} aria-invalid={!!portCheck.problem} aria-describedby={`${uid}-ports`} />
          </Field>
          <ListNote id={`${uid}-ports`} checked={portCheck} noun="port" />
        </div>
      </div>
      <div className="grid gap-x-8 md:grid-cols-2">
        <Check checked={torrent} onChange={setTorrent}>
          Block BitTorrent
        </Check>
        <Check checked={smtp} onChange={setSmtp}>
          Block outgoing mail (port 25)
        </Check>
      </div>
      <Note>Local networks, the server itself and cloud metadata addresses are always blocked for VPN users.</Note>
      {error && <Note tone="bad">{error}</Note>}
      <div className="flex justify-end">
        <Button type="submit" variant="cta" disabled={busy || invalid}>
          {busy ? 'Saving…' : saveLabel}
        </Button>
      </div>
    </form>
  );
}
