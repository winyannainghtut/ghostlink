// How long toasts stay (shared/toast.ts), code review 2026-11-25, U13: long notices were gone in
// 3.2 s. Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { timeLeft, toastDuration } from '../shared/toast.ts';

test('short toasts keep their time, errors stay longer', () => {
  assert.equal(toastDuration('Key copied.', 'ok'), 3200);
  assert.equal(toastDuration('Saved.', 'info'), 3200);
  assert.equal(toastDuration('Could not copy.', 'bad'), 6000);
});

test('a notice of several sentences stays long enough to read', () => {
  // The notice after turning WireGuard on (admin/protocols.ts), about 100 characters.
  const notice = "WireGuard is on. Allow UDP 51820 in this server's firewall and your cloud security group.";
  assert.ok(toastDuration(notice, 'ok') > 6000, String(toastDuration(notice, 'ok')));
  const long = 'x'.repeat(400);
  assert.equal(toastDuration(long, 'ok'), 20000);
  assert.equal(toastDuration(long, 'bad'), 20000);
  // Longer text never gets less time than shorter text.
  let last = 0;
  for (let n = 0; n <= 500; n += 25) {
    const ms = toastDuration('x'.repeat(n), 'ok');
    assert.ok(ms >= last);
    last = ms;
  }
});

test('after a pause a toast has at least a moment left', () => {
  assert.equal(timeLeft(3200, 1000), 2200);
  assert.equal(timeLeft(3200, 3100), 1500);
  assert.equal(timeLeft(3200, 9000), 1500);
});
