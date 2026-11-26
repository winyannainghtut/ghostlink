// "Connect now" after "Use on this device" (admin/connectHere.ts, desktop/src/connectNow.ts), and the
// toast that offers it (shared/toast.ts). Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { bindApi } from '../admin/api.ts';
import { ActiveElsewhere, connectHere, switchQuestion } from '../admin/connectHere.ts';
import { activeElsewhere, startOutcome } from '../desktop/src/connectNow.ts';
import { ServiceError } from '../desktop/src/ipc.ts';
import { toastDuration } from '../shared/toast.ts';

// The pieces connectHere is given: what it was asked to connect to, what it asked the person, what it said.
function deps({ connect, answer = true } = {}) {
  const log = { connects: [], asked: [], toasts: [] };
  return {
    log,
    deps: {
      connect: async (profile, options) => {
        log.connects.push([profile, options?.switch ?? false]);
        if (connect) await connect(profile, options);
      },
      confirm: async ask => (log.asked.push(ask), answer),
      toast: (message, tone) => log.toasts.push([message, tone ?? 'ok']),
    },
  };
}

test('with nothing else connected, Connect now starts the tunnel and says it is connecting', async () => {
  const { deps: d, log } = deps();
  assert.equal(await connectHere(d, 'maya-wireguard-3', 'Maya'), 'started');
  assert.deepEqual(log.connects, [['maya-wireguard-3', false]]);
  assert.deepEqual(log.asked, []);
  assert.deepEqual(log.toasts, [['Connecting to Maya…', 'info']]);
});

test('with another server connected the person is asked, and a yes switches', async () => {
  const { deps: d, log } = deps({
    connect: async (_profile, options) => {
      if (!options?.switch) throw new ActiveElsewhere('Home A');
    },
  });
  assert.equal(await connectHere(d, 'maya-wireguard-3', 'Maya'), 'started');
  assert.deepEqual(log.connects, [['maya-wireguard-3', false], ['maya-wireguard-3', true]]);
  assert.equal(log.asked.length, 1);
  assert.deepEqual(log.asked[0], switchQuestion('Home A', 'Maya'));
  assert.match(log.asked[0].text, /Home A is connected now/);
  assert.match(log.asked[0].text, /disconnects it first/);
});

test('a no leaves the connection as it is, and says nothing more', async () => {
  const { deps: d, log } = deps({ answer: false, connect: async () => { throw new ActiveElsewhere('Home A'); } });
  assert.equal(await connectHere(d, 'maya-wireguard-3', 'Maya'), 'declined');
  assert.deepEqual(log.connects, [['maya-wireguard-3', false]], 'no switch was sent');
  assert.deepEqual(log.toasts, []);
});

test('any other refusal is shown as the service worded it, and nothing is switched', async () => {
  const { deps: d, log } = deps({ connect: async () => { throw new Error('The saved key is invalid, unsupported or its dynamic configuration is unavailable'); } });
  assert.equal(await connectHere(d, 'maya-wireguard-3', 'Maya'), 'failed');
  assert.deepEqual(log.asked, []);
  assert.deepEqual(log.toasts, [['The saved key is invalid, unsupported or its dynamic configuration is unavailable', 'bad']]);
});

test('a switch the service then refuses is reported, not retried', async () => {
  const { deps: d, log } = deps({
    connect: async (_profile, options) => {
      throw options?.switch ? new Error('Network cleanup is not verified') : new ActiveElsewhere('Home A');
    },
  });
  assert.equal(await connectHere(d, 'maya-wireguard-3', 'Maya'), 'failed');
  assert.equal(log.connects.length, 2);
  assert.deepEqual(log.toasts, [['Network cleanup is not verified', 'bad']]);
});

test('the service\'s "busy" about another active server is told apart from other "busy" answers', () => {
  assert.equal(activeElsewhere('busy', 'Another server is active; use switch'), true);
  assert.equal(activeElsewhere('busy', 'Another profile is active; use switch'), true);
  assert.equal(activeElsewhere('busy', 'Disconnect before replacing the active access key'), false);
  assert.equal(activeElsewhere('not_found', 'Another server is active; use switch'), false);
  assert.equal(activeElsewhere(undefined, undefined), false);
  // What the desktop's request() throws for it.
  const error = new ServiceError('Another server is active; use switch', 'busy');
  assert.equal(error.code, 'busy');
  assert.ok(error instanceof Error);
  assert.equal(activeElsewhere(error.code, error.message), true);
});

test('an answer that started nothing is a failure, and a connecting one is a start', () => {
  assert.deepEqual(startOutcome({ status: 'connecting', last_error: null }), { kind: 'started' });
  assert.deepEqual(startOutcome({ status: 'connected' }), { kind: 'started' });
  assert.deepEqual(startOutcome({ status: 'failed', last_error: 'connection_timeout' }), { kind: 'failed', code: 'connection_timeout' });
  assert.deepEqual(startOutcome({ status: 'disconnected', last_error: null }), { kind: 'failed', code: null });
  assert.deepEqual(startOutcome(null), { kind: 'started' }, 'an answer with nothing in it is not a failure the service reported');
});

test('only the desktop transport can connect, and says which server a key was saved under', async () => {
  const plain = bindApi({ call: async () => ({}), text: async () => '', save: async () => null });
  assert.equal(plain.canConnect, false);
  assert.equal(plain.desktop, false);
  await assert.rejects(plain.connect('x'), /Only the desktop app/);
  const connected = [];
  const desktop = bindApi({ call: async () => ({}), text: async () => '', save: async () => null, desktop: true, useHere: async () => 'maya-vless-7', connect: async (profile, options) => void connected.push([profile, options]) });
  assert.equal(desktop.canConnect, true);
  assert.equal(desktop.desktop, true);
  assert.equal(await desktop.useHere({}, {}), 'maya-vless-7');
  await desktop.connect('maya-vless-7', { switch: true });
  assert.deepEqual(connected, [['maya-vless-7', { switch: true }]]);
});

test('a toast with a button stays long enough to be used', () => {
  assert.equal(toastDuration('Key copied.', 'ok'), 3200, 'unchanged without a button');
  assert.equal(toastDuration('Saved.', 'ok', true), 10000);
  assert.equal(toastDuration('Saved.', 'bad', true), 6000, 'an error keeps its own time');
  assert.equal(toastDuration('x'.repeat(400), 'ok', true), 20000);
});
