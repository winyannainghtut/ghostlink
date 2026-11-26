// Replacing the management key from the server settings (admin/rotateKey.ts): what the person is
// told, and what each answer means. Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { toOp } from '../desktop/src/routes.ts';
import { rotateWarning, rotationResult } from '../admin/rotateKey.ts';

test('the warning says what happens to the old key and where the new one goes, in each place', () => {
  for (const where of ['app', 'dashboard', 'shown']) {
    const text = rotateWarning(where);
    assert.match(text, /old management key/, where);
    assert.match(text, /every device and script|any device or script/, where);
    assert.match(text, /sudo ghostlink-server show-key/, `${where}: how other devices get the new key`);
  }
  assert.match(rotateWarning('app'), /stops working at once/);
  assert.match(rotateWarning('app'), /new key is saved on this device/);
  assert.match(rotateWarning('dashboard'), /stops working at once/);
  assert.match(rotateWarning('dashboard'), /shown once/);
  assert.match(rotateWarning('shown'), /only time/);
  assert.doesNotMatch(rotateWarning('dashboard'), /saved on this device/, 'the dashboard saves nothing on a device');
});

test('the app: the service says it rotated and sealed the key; there is no key to show', () => {
  const answer = rotationResult({ rotated: true, server: { name: 'Home A' } });
  assert.equal(answer.kind, 'saved');
  assert.match(answer.message, /new key is saved on this device/);
  assert.match(answer.message, /show-key/);
  assert.equal('key' in answer, false);
});

test('the dashboard: the server answers with the new key, which is shown once as the text the app takes', () => {
  const answer = rotationResult({ management_key: { apiUrl: 'https://203.0.113.10:8443/abcDEF', certSha256: 'AB:CD' } });
  assert.equal(answer.kind, 'shown');
  assert.deepEqual(JSON.parse(answer.key), { apiUrl: 'https://203.0.113.10:8443/abcDEF', certSha256: 'AB:CD' });
  // Only these two fields, in the order show-key prints them; nothing else the server sent.
  assert.equal(answer.key, '{"apiUrl":"https://203.0.113.10:8443/abcDEF","certSha256":"AB:CD"}');
  const extra = rotationResult({ management_key: { apiUrl: 'https://203.0.113.10:8443/x', certSha256: 'AB', note: 'ignored' }, other: 1 });
  assert.equal(extra.key, '{"apiUrl":"https://203.0.113.10:8443/x","certSha256":"AB"}');
});

test('any other answer is not taken for a rotated key, and says how to get the key that works', () => {
  for (const answer of [undefined, null, 'ok', 5, {}, { rotated: false }, { rotated: 'yes' }, { management_key: null }, { management_key: {} }, { management_key: { apiUrl: 'https://203.0.113.10:8443/x' } }, { management_key: { apiUrl: '', certSha256: 'AB' } }]) {
    const result = rotationResult(answer);
    assert.equal(result.kind, 'unclear', JSON.stringify(answer));
    assert.match(result.message, /show-key/);
    assert.equal('key' in result, false);
  }
});

test('the app sends the service {op: rotate_key}, and only for a POST', () => {
  assert.deepEqual(toOp('POST', '/server/rotate-key'), { op: 'rotate_key' });
  assert.deepEqual(toOp('POST', '/server/rotate-key', {}), { op: 'rotate_key' });
  for (const method of ['GET', 'PUT', 'PATCH', 'DELETE']) assert.throws(() => toOp(method, '/server/rotate-key'), /cannot do/, method);
  assert.throws(() => toOp('POST', '/server/rotate-key/extra'), /cannot do/);
});
