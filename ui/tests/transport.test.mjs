// The desktop's managed-server transport (desktop/src/serverTransport.ts) against a stand-in for the
// local service: what "Use on this device" returns, how "Connect now" asks the service and reads its
// answers, and the rotate-key request. Vite loads the TypeScript (tests/support/render.mjs); the
// Tauri bridge the app talks through is replaced by a function the tests control. Run:
// node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { startRenderer } from './support/render.mjs';

// What the service is asked, and what it answers (by action type).
const sent = [];
let answers = {};
globalThis.__TAURI__ = {
  core: {
    invoke: async (command, args) => {
      assert.equal(command, 'vpn_request');
      sent.push(args.action);
      const answer = answers[args.action.type];
      return typeof answer === 'function' ? answer(args.action) : answer;
    },
  },
};
const ok = data => ({ success: true, data });
const no = (code, message) => ({ success: false, error: { code, message } });

let ui, transport, ActiveElsewhere;
before(async () => {
  ui = await startRenderer();
  ({ ActiveElsewhere } = await ui.load('/admin/connectHere.ts'));
  const { serverTransport } = await ui.load('/desktop/src/serverTransport.ts');
  transport = serverTransport('a', () => imported++);
});
after(() => ui?.close());
let imported = 0;
const user = { id: '1', name: 'Maya' };
const key = { id: '7', protocol: 'shadowsocks', access_key: 'ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTp0ZXN0@203.0.113.12:8388/?outline=1' };

test('Use on this device answers the ID the key was saved under', async () => {
  sent.length = 0;
  answers = { profile_list: ok({ profiles: [{ name: 'maya-shadowsocks-7' }] }), access_key_import: action => ok({ name: action.name, kind: 'outline' }) };
  const id = await transport.useHere(user, key);
  // The first ID was taken, so the service was asked for another one, and the answer is that one.
  const asked = sent.find(a => a.type === 'access_key_import');
  assert.notEqual(asked.name, 'maya-shadowsocks-7');
  assert.match(asked.name, /^maya-shadowsocks-7-/);
  assert.equal(asked.access_key, key.access_key);
  assert.equal(id, asked.name);
  assert.equal(imported, 1, 'the list of servers is refreshed');
  // An answer without a name falls back to the ID that was asked for.
  answers = { profile_list: ok({ profiles: [] }), access_key_import: ok(null) };
  assert.equal(await transport.useHere(user, key), 'maya-shadowsocks-7');
});

test('Connect now asks the service to connect, and a switch asks it to switch', async () => {
  sent.length = 0;
  answers = { connect: ok({ status: 'connecting', profile: 'maya-shadowsocks-7', last_error: null }), switch: ok({ status: 'connecting', profile: 'maya-shadowsocks-7', last_error: null }) };
  await transport.connect('maya-shadowsocks-7');
  await transport.connect('maya-shadowsocks-7', { switch: true });
  assert.deepEqual(sent, [
    { type: 'connect', profile: 'maya-shadowsocks-7' },
    { type: 'switch', profile: 'maya-shadowsocks-7' },
  ]);
});

test('with another server active, the service says busy and the person is told which one', async () => {
  answers = {
    connect: no('busy', 'Another server is active; use switch'),
    info: ok({ connection: { status: 'connected', profile: 'home-a' } }),
    profile_list: ok({ profiles: [{ name: 'home-a', display_name: 'Home A' }, { name: 'maya-shadowsocks-7', display_name: 'Maya' }] }),
  };
  await assert.rejects(transport.connect('maya-shadowsocks-7'), error => {
    assert.ok(error instanceof ActiveElsewhere);
    assert.equal(error.active, 'Home A');
    return true;
  });
  // Unknown who is active: still the question, with a general name.
  answers = { ...answers, info: no('internal', 'x') };
  await assert.rejects(transport.connect('maya-shadowsocks-7'), error => error instanceof ActiveElsewhere && error.active === 'Another server');
});

test('other answers are shown as the service worded them', async () => {
  answers = { connect: no('busy', 'Disconnect before replacing the active access key') };
  await assert.rejects(transport.connect('x'), error => !(error instanceof ActiveElsewhere) && /Disconnect before replacing/.test(error.message));
  answers = { connect: no('invalid_profile', 'The saved key is invalid, unsupported or its dynamic configuration is unavailable') };
  await assert.rejects(transport.connect('x'), /saved key is invalid/);
  // A start that reports itself failed is explained in words, not as a code.
  answers = { connect: ok({ status: 'failed', last_error: 'outline_engine_start_failed_check_permissions_or_driver' }) };
  await assert.rejects(transport.connect('x'), /could not start/);
});

test('rotating the management key sends rotate_key and returns what the service answered', async () => {
  sent.length = 0;
  answers = { server_call: ok({ rotated: true, server: { name: 'Home A' } }) };
  const answer = await transport.call('POST', '/server/rotate-key');
  assert.deepEqual(sent, [{ type: 'server_call', server: 'a', op: { op: 'rotate_key' } }]);
  assert.deepEqual(answer, { rotated: true, server: { name: 'Home A' } });
  assert.equal(transport.desktop, true);
});
