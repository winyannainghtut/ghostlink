// The order of the client list and the protocols on a client's card (admin/clients.ts), code
// review 2026-11-25, U11. Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { protocolsOf, sortClients } from '../admin/clients.ts';

const user = (id, name, usage = 0, enabled = true) => ({ id, name, usage_bytes: usage, enabled });
const users = [user('1', 'maya phone', 5_000_000), user('2', 'Studio Mac', 90_000_000), user('3', 'office router', 0), user('4', 'Backup NAS', 2_000, false), user('10', 'Zed', 1)];
const live = {
  '1': { id: '1', online: true, down_bps: 1_000_000, up_bps: 50_000, last_active_unix: 1_000_300 },
  '2': { id: '2', online: false, down_bps: 0, up_bps: 0, last_active_unix: 1_000_100 },
  '3': { id: '3', online: true, down_bps: 20_000, up_bps: 20_000, last_active_unix: 1_000_299 },
};
const status = u => (!u.enabled ? 'paused' : live[u.id]?.online ? 'online' : 'idle');
const names = list => list.map(u => u.name);

test('clients are listed as the server has them unless another order is chosen', () => {
  assert.deepEqual(names(sortClients(users, 'added', live, status)), names(users));
});

test('by name, ignoring case and counting numbers as numbers', () => {
  assert.deepEqual(names(sortClients(users, 'name', live, status)), ['Backup NAS', 'maya phone', 'office router', 'Studio Mac', 'Zed']);
  const numbered = [user('1', 'phone 10'), user('2', 'phone 9'), user('3', 'Phone 2')];
  assert.deepEqual(names(sortClients(numbered, 'name', {}, status)), ['Phone 2', 'phone 9', 'phone 10']);
});

test('by status: online first, paused last, ties by name', () => {
  assert.deepEqual(names(sortClients(users, 'status', live, status)), ['maya phone', 'office router', 'Studio Mac', 'Zed', 'Backup NAS']);
});

test('by live speed, the busiest first; a client with no reading counts as idle', () => {
  assert.deepEqual(names(sortClients(users, 'speed', live, status)), ['maya phone', 'office router', 'Backup NAS', 'Studio Mac', 'Zed']);
});

test('by data used, the most first', () => {
  assert.deepEqual(names(sortClients(users, 'usage', live, status)), ['Studio Mac', 'maya phone', 'Backup NAS', 'Zed', 'office router']);
});

test('by last seen, the newest first, never seen last', () => {
  assert.deepEqual(names(sortClients(users, 'seen', live, status)), ['maya phone', 'office router', 'Studio Mac', 'Backup NAS', 'Zed']);
});

test('sorting leaves the list it was given alone', () => {
  const copy = names(users);
  sortClients(users, 'usage', live, status);
  assert.deepEqual(names(users), copy);
});

test('a client with several keys shows each protocol once', () => {
  assert.deepEqual(protocolsOf([{ protocol: 'wireguard' }, { protocol: 'vless' }, { protocol: 'wireguard' }, { protocol: 'ikev2' }]), ['wireguard', 'vless', 'ikev2']);
  assert.deepEqual(protocolsOf([]), []);
});
