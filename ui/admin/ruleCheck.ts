// Checks of the lists in the rules and split-tunneling editors, line by line, so a mistake is shown
// beside the list while it is typed and not as an error after Save. They follow what the servers
// accept (core/outline-worker/internal/server/rules.go for the rules of a server; core/vpn-engine
// split.rs for the client's split tunneling), and where the two differ they accept what either
// would: a check that refuses an entry the server takes would be worse than none. The server stays
// the judge. No imports, so the tests run it directly (node --experimental-strip-types).

export type Problem = { line: number; entry: string; reason: string };
export type Checked = { count: number; problem: Problem | null };

export const MAX_ENTRIES = 512;
export const MAX_APPS = 128;

function ipv4(text: string): boolean {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(text);
  return !!m && m.slice(1).every(o => Number(o) <= 255 && (o === '0' || !o.startsWith('0')));
}

// IPv6 text: hex groups, one "::" at most, an IPv4 tail allowed. Without "::" there are exactly
// eight groups; with it the count is not held to the letter (the parsers differ on the edge).
function ipv6(text: string): boolean {
  if (!/^[0-9a-f:.]+$/i.test(text) || !text.includes(':') || text.includes(':::')) return false;
  const parts = text.split('::');
  if (parts.length > 2) return false;
  const groups = parts.flatMap(part => (part === '' ? [] : part.split(':')));
  let count = 0;
  for (let i = 0; i < groups.length; i++) {
    const group = groups[i];
    if (group.includes('.')) {
      if (i !== groups.length - 1 || !ipv4(group)) return false;
      count += 2;
    } else {
      if (!/^[0-9a-f]{1,4}$/i.test(group)) return false;
      count += 1;
    }
  }
  return parts.length === 2 ? count <= 8 : count === 8;
}

const isIP = (text: string) => ipv4(text) || ipv6(text);

// An address or a range such as 203.0.113.0/24 or 2001:db8::/32.
function isAddressOrRange(text: string): boolean {
  const slash = text.indexOf('/');
  if (slash < 0) return isIP(text);
  const address = text.slice(0, slash), bits = text.slice(slash + 1);
  if (bits !== '' && !/^\d{1,3}$/.test(bits)) return false;
  const length = bits === '' ? 0 : Number(bits);
  return (ipv4(address) && length <= 32) || (ipv6(address) && length <= 128);
}

function isPort(text: string): boolean {
  if (!/^\d{1,5}$/.test(text)) return false;
  const n = Number(text);
  return n >= 1 && n <= 65535;
}

// A port such as 25, or a range such as 6881-6889 (spaces around the dash are fine).
function isPortOrRange(text: string): boolean {
  const dash = text.indexOf('-');
  if (dash < 0) return isPort(text.trim());
  const low = text.slice(0, dash).trim(), high = text.slice(dash + 1).trim();
  return isPort(low) && isPort(high) && Number(high) >= Number(low);
}

// A site name as the servers read it: example.com, *.example.com or .example.com all mean the
// site and its subdomains. `underscore`: the server's rules allow "_" in a label, split does not.
function isSite(text: string, underscore: boolean): boolean {
  const name = text.trim().toLowerCase().replace(/^\*\./, '').replace(/^\./, '').replace(/\.$/, '');
  if (!name || name.length > 253) return false;
  const allowed = underscore ? /^[a-z0-9_-]+$/ : /^[a-z0-9-]+$/;
  return name.split('.').every(label => label.length > 0 && label.length <= 63 && !label.startsWith('-') && !label.endsWith('-') && allowed.test(label));
}

function check(lines: string[], limit: number, what: string, valid: (entry: string) => boolean, reason: (entry: string) => string): Checked {
  const bad = lines.findIndex(entry => !valid(entry));
  if (bad >= 0) return { count: lines.length, problem: { line: bad + 1, entry: lines[bad], reason: reason(lines[bad]) } };
  if (lines.length > limit) return { count: lines.length, problem: { line: 0, entry: '', reason: `At most ${limit} ${what} are supported, and there are ${lines.length}.` } };
  return { count: lines.length, problem: null };
}

// `lines` are the non-empty, trimmed lines of the field (shared/format.ts, lines()).
// `kind` is where the list goes: the server's rules or the client's split tunneling.
export function checkSites(lines: string[], kind: 'rules' | 'split'): Checked {
  return check(
    lines,
    MAX_ENTRIES,
    'sites',
    entry => isSite(entry, kind === 'rules') && !(kind === 'rules' && isIP(entry.trim().toLowerCase())),
    entry => (isIP(entry.trim().toLowerCase()) && kind === 'rules' ? `“${entry}” is an IP address: put it under blocked addresses.` : `“${entry}” is not a site name. Use a name such as example.com.`),
  );
}

export function checkAddresses(lines: string[]): Checked {
  return check(lines, MAX_ENTRIES, 'addresses', isAddressOrRange, entry => `“${entry}” is not an IP address or a range such as 203.0.113.0/24.`);
}

export function checkPorts(lines: string[]): Checked {
  return check(lines, MAX_ENTRIES, 'ports', isPortOrRange, entry => `“${entry}” is not a port (1 to 65535) or a range such as 6881-6889.`);
}

// An app is an executable name or a full path: any text of up to 260 characters without control
// characters.
export function checkApps(lines: string[]): Checked {
  return check(
    lines,
    MAX_APPS,
    'apps',
    entry => entry.length <= 260 && !/[\u0000-\u001f\u007f-\u009f]/.test(entry),
    entry => `“${entry.slice(0, 40)}” is too long or has a control character: use an executable name such as chrome.exe or a full path.`,
  );
}

// The sentence beside a list: what is wrong, or how many entries it has ("2 addresses").
export function describe(checked: Checked, noun: string, many = noun + 's'): { ok: boolean; text: string } {
  if (checked.problem) return { ok: false, text: checked.problem.line ? `Line ${checked.problem.line}: ${checked.problem.reason}` : checked.problem.reason };
  return { ok: true, text: checked.count ? `${checked.count} ${checked.count === 1 ? noun : many}` : '' };
}
