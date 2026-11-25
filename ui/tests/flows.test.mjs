// Buttons that change something: one run at a time (shared/lock.ts), and flows of several steps that
// resume where they failed instead of repeating the first step (shared/steps.ts) - the new client
// whose key failed, and the saved server whose name failed (code review 2026-11-25, U4).
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { exclusive } from '../shared/lock.ts';
import { step } from '../shared/steps.ts';

const settle = () => new Promise(resolve => setImmediate(resolve));

test('a second click while a save is running does not start a second save', async () => {
  const lock = { running: false };
  let started = 0;
  let finish;
  const work = () => new Promise(resolve => ((started += 1), (finish = resolve)));
  const first = exclusive(lock, work);
  // Same instant: the page has not drawn the disabled button yet.
  const second = exclusive(lock, work);
  assert.equal(await second, undefined);
  assert.equal(started, 1);
  finish('saved');
  assert.equal(await first, 'saved');
  // Done: the button works again.
  const third = exclusive(lock, async () => 'again');
  assert.equal(await third, 'again');
});

test('the lock is released when the work fails, so the user can try again', async () => {
  const lock = { running: false };
  await assert.rejects(exclusive(lock, async () => Promise.reject(new Error('the server said no'))), /said no/);
  assert.equal(lock.running, false);
  assert.equal(await exclusive(lock, async () => 'worked'), 'worked');
});

// The new client sheet, as it uses steps: the client first, then its key.
function newClient(api, done) {
  return (async () => {
    const user = await step(done, 'user', () => api('POST', '/users'));
    const key = await step(done, 'key', () => api('POST', `/users/${user.id}/keys`));
    return { user, key };
  })();
}

test('a new client whose key failed gets its key on the next try, not a second client', async () => {
  const calls = [];
  let keyFails = true;
  const api = async (method, path) => {
    calls.push(`${method} ${path}`);
    if (path === '/users') return { id: '7' };
    if (keyFails) throw new Error('shadowsocks is off');
    return { id: '3' };
  };
  const done = new Map();
  await assert.rejects(newClient(api, done), /shadowsocks is off/);
  keyFails = false;
  const result = await newClient(api, done);
  assert.deepEqual(result, { user: { id: '7' }, key: { id: '3' } });
  // The old sheet sent POST /users again here, which the server refuses: the name is taken.
  assert.deepEqual(calls, ['POST /users', 'POST /users/7/keys', 'POST /users/7/keys']);
});

test('a server whose name could not be saved is not imported again', async () => {
  const calls = [];
  let renameFails = true;
  const done = new Map();
  const add = async () => {
    const id = await step(done, 'import', async () => (calls.push('import'), 'home-a'));
    calls.push('rename');
    if (renameFails) throw new Error('the service is busy');
    return id;
  };
  await assert.rejects(add(), /busy/);
  renameFails = false;
  assert.equal(await add(), 'home-a');
  assert.deepEqual(calls, ['import', 'rename', 'rename']);
});

test('a step that failed is tried again, and only the steps that worked are kept', async () => {
  const done = new Map();
  let tries = 0;
  const flaky = () => step(done, 'first', async () => (++tries < 2 ? Promise.reject(new Error('timeout')) : 'ok'));
  await assert.rejects(flaky(), /timeout/);
  assert.equal(done.has('first'), false);
  assert.equal(await flaky(), 'ok');
  assert.equal(await flaky(), 'ok');
  assert.equal(tries, 2);
  await settle();
});
