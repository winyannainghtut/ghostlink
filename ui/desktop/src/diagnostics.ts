// The service's diagnostics and audit log as rows a person can read, and as plain text to copy into a
// message. The raw answers are what `doctor` and `logs` return (services/vpn-daemon/src/main.rs).
// No imports, so the tests run it directly (node --experimental-strip-types).

export type Doctor = {
  engine_available?: boolean;
  outline_engine_available?: boolean;
  openvpn_engine_version?: string | null;
  ipc?: string;
  profile_count?: number;
  connection?: { status?: string; profile?: string | null; protocol?: string | null; last_error?: string | null } | null;
  checks?: Record<string, string>;
  limitations?: string[];
};
export type LogEntry = { timestamp_unix: number; actor: string; operation: string; outcome: string };

export type Tone = 'ok' | 'bad' | 'warn' | 'muted';
export type Row = { label: string; value: string; tone: Tone };
export type Section = { title: string; rows: Row[] };

const CHECKS: Record<string, string> = { routed_traffic: 'Routed traffic', dns_leaks: 'DNS leaks', ipv6_leaks: 'IPv6 leaks', kill_switch: 'Kill switch' };
const CHECK_STATES: Record<string, { text: string; tone: Tone }> = {
  not_tested: { text: 'Not tested', tone: 'warn' },
  unsupported: { text: 'Not supported', tone: 'warn' },
  passed: { text: 'Passed', tone: 'ok' },
  failed: { text: 'Failed', tone: 'bad' },
};
const IPC: Record<string, string> = { os_user_restricted: 'Only your own account can use it' };

// What the service reports, in groups. `states` names the connection states, `failure` says why a
// connection failed in words (the app's own wording, desktop/src/failures.ts).
export function doctorSections(doctor: Doctor, states: Record<string, string>, failure: (code: string | null | undefined) => string): Section[] {
  const connection = doctor.connection;
  const status = connection?.status;
  const engines: Row[] = [
    { label: 'Tunnel engine', value: doctor.outline_engine_available ? 'Available' : 'Not found', tone: doctor.outline_engine_available ? 'ok' : 'bad' },
    { label: 'OpenVPN engine', value: doctor.openvpn_engine_version || 'Not installed', tone: doctor.openvpn_engine_version ? 'ok' : 'muted' },
  ];
  const service: Row[] = [
    { label: 'Local access', value: (doctor.ipc && IPC[doctor.ipc]) || doctor.ipc || 'Unknown', tone: 'muted' },
    { label: 'Servers on this device', value: String(doctor.profile_count ?? 0), tone: 'muted' },
    { label: 'Connection', value: (status && states[status]) || status || 'Unknown', tone: status === 'connected' ? 'ok' : status === 'failed' ? 'bad' : 'muted' },
    ...(connection?.profile ? [{ label: 'Server', value: connection.profile, tone: 'muted' as Tone }] : []),
    ...(status === 'failed' && connection?.last_error ? [{ label: 'Last problem', value: failure(connection.last_error), tone: 'bad' as Tone }] : []),
  ];
  const checks: Row[] = Object.entries(doctor.checks ?? {}).map(([key, state]) => ({
    label: CHECKS[key] ?? key.replace(/_/g, ' '),
    value: CHECK_STATES[state]?.text ?? state.replace(/_/g, ' '),
    tone: CHECK_STATES[state]?.tone ?? 'muted',
  }));
  return [
    { title: 'Engines', rows: engines },
    { title: 'Service', rows: service },
    ...(checks.length ? [{ title: 'Protection checks', rows: checks }] : []),
  ];
}

export function doctorText(doctor: Doctor, version: string, states: Record<string, string>, failure: (code: string | null | undefined) => string): string {
  const out = [`Ghostlink diagnostics, app ${version}`];
  for (const section of doctorSections(doctor, states, failure)) {
    out.push('', `${section.title}:`, ...section.rows.map(r => `  ${r.label}: ${r.value}`));
  }
  if (doctor.limitations?.length) out.push('', 'Limits:', ...doctor.limitations.map(l => `  - ${l}`));
  return out.join('\n');
}

// "2026-11-25 14:03:07": the time of an entry, in the reader's time zone unless one is given.
export function logTime(unix: number, locale?: string, timeZone?: string): string {
  return new Date(unix * 1000).toLocaleString(locale, { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false, timeZone });
}

// Outcomes that mean something went wrong.
export const isBadOutcome = (outcome: string) => /denied|fail|error|refused|unverified/i.test(outcome);

// The newest entries first, the way a person reads a log.
export function logRows(entries: LogEntry[], limit = 1000): LogEntry[] {
  return [...entries].reverse().slice(0, limit);
}

export function logText(entries: LogEntry[], locale?: string, timeZone?: string): string {
  return logRows(entries)
    .map(e => `${logTime(e.timestamp_unix, locale, timeZone)}  ${e.actor}  ${e.operation}  ${e.outcome}`)
    .join('\n');
}
