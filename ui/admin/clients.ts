// How the client list is ordered, and which protocols a client's keys use. No imports (types
// only), so the tests run it directly (node --experimental-strip-types).
import type { KeySummary, LiveUser, User } from './api';

export type SortKey = 'added' | 'name' | 'status' | 'speed' | 'usage' | 'seen';
export const SORT_KEYS: SortKey[] = ['added', 'name', 'status', 'speed', 'usage', 'seen'];
export const SORT_LABELS: Record<SortKey, string> = {
  added: 'Oldest first',
  name: 'Name',
  status: 'Status',
  speed: 'Live speed',
  usage: 'Data used',
  seen: 'Last seen',
};

type Status = 'online' | 'idle' | 'paused' | 'limit' | 'pending' | 'offline';
// Online clients first, then those that may connect and are not, then the ones that cannot.
const STATUS_RANK: Record<Status, number> = { online: 0, pending: 1, idle: 2, limit: 3, paused: 4, offline: 5 };

// A sorted copy. Ties fall back to the name, so the order never shuffles between refreshes; a client
// the live readings do not know counts as having no traffic and never having been seen.
export function sortClients(users: readonly User[], key: SortKey, live: Record<string, LiveUser | undefined>, statusOf: (user: User) => Status): User[] {
  const byName = (a: User, b: User) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' }) || a.id.localeCompare(b.id, undefined, { numeric: true });
  const rate = (u: User) => (live[u.id]?.down_bps ?? 0) + (live[u.id]?.up_bps ?? 0);
  const seen = (u: User) => live[u.id]?.last_active_unix ?? 0;
  const compare: Record<SortKey, (a: User, b: User) => number> = {
    added: () => 0,
    name: byName,
    status: (a, b) => STATUS_RANK[statusOf(a)] - STATUS_RANK[statusOf(b)],
    speed: (a, b) => rate(b) - rate(a),
    usage: (a, b) => b.usage_bytes - a.usage_bytes,
    seen: (a, b) => seen(b) - seen(a),
  };
  // Array.prototype.sort is stable: "added" keeps the server's order.
  return [...users].sort((a, b) => compare[key](a, b) || (key === 'added' ? 0 : byName(a, b)));
}

// The protocols a client's keys use, each once, in the order of the first key that uses it.
export function protocolsOf(keys: readonly Pick<KeySummary, 'protocol'>[]): string[] {
  return [...new Set(keys.map(k => k.protocol))];
}
