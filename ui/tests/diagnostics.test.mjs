// The diagnostics and the service log as rows and as text to copy (desktop/src/diagnostics.ts), code
// review 2026-11-25, U8. The sample is what the service's `doctor` answers (services/vpn-daemon).
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { doctorSections, doctorText, isBadOutcome, logRows, logText, logTime } from '../desktop/src/diagnostics.ts';

const states = { connected: 'Connected', disconnected: 'Disconnected', failed: 'Connection failed' };
const failure = code => (code === 'connection_timeout' ? 'The server did not answer in time.' : 'Check your key.');
const doctor = {
  engine_available: true,
  outline_engine_available: true,
  openvpn_engine_version: null,
  ipc: 'os_user_restricted',
  profile_count: 3,
  connection: { status: 'disconnected', profile: null, protocol: null, last_error: null },
  checks: { routed_traffic: 'not_tested', dns_leaks: 'not_tested', ipv6_leaks: 'not_tested', kill_switch: 'unsupported' },
  limitations: ['No persistent kill switch or independently verified leak-prevention guarantee'],
};
const row = (sections, title, label) => sections.find(s => s.title === title).rows.find(r => r.label === label);

test('the diagnostics read as groups of rows, not as JSON', () => {
  const sections = doctorSections(doctor, states, failure);
  assert.deepEqual(sections.map(s => s.title), ['Engines', 'Service', 'Protection checks']);
  assert.deepEqual(row(sections, 'Engines', 'Tunnel engine'), { label: 'Tunnel engine', value: 'Available', tone: 'ok' });
  assert.deepEqual(row(sections, 'Engines', 'OpenVPN engine'), { label: 'OpenVPN engine', value: 'Not installed', tone: 'muted' });
  assert.equal(row(sections, 'Service', 'Servers on this device').value, '3');
  assert.equal(row(sections, 'Service', 'Local access').value, 'Only your own account can use it');
  assert.equal(row(sections, 'Service', 'Connection').value, 'Disconnected');
  assert.deepEqual(row(sections, 'Protection checks', 'Kill switch'), { label: 'Kill switch', value: 'Not supported', tone: 'warn' });
  assert.deepEqual(row(sections, 'Protection checks', 'DNS leaks'), { label: 'DNS leaks', value: 'Not tested', tone: 'warn' });
});

test('a missing engine and a failed connection stand out', () => {
  const broken = { ...doctor, outline_engine_available: false, openvpn_engine_version: 'OpenVPN 2.6.12 x86_64', connection: { status: 'failed', profile: 'home', last_error: 'connection_timeout' } };
  const sections = doctorSections(broken, states, failure);
  assert.equal(row(sections, 'Engines', 'Tunnel engine').tone, 'bad');
  assert.equal(row(sections, 'Engines', 'OpenVPN engine').value, 'OpenVPN 2.6.12 x86_64');
  assert.deepEqual(row(sections, 'Service', 'Last problem'), { label: 'Last problem', value: 'The server did not answer in time.', tone: 'bad' });
  assert.equal(row(sections, 'Service', 'Server').value, 'home');
});

test('an answer with unknown or missing parts is still shown', () => {
  const sections = doctorSections({ checks: { something_new: 'maybe_later' } }, states, failure);
  assert.equal(row(sections, 'Protection checks', 'something new').value, 'maybe later');
  assert.equal(row(sections, 'Engines', 'Tunnel engine').value, 'Not found');
  assert.deepEqual(doctorSections({}, states, failure).map(s => s.title), ['Engines', 'Service']);
});

test('the report to copy has every row and the limits, and no JSON', () => {
  const text = doctorText(doctor, '0.9.1', states, failure);
  assert.match(text, /^Ghostlink diagnostics, app 0\.9\.1\n/);
  assert.match(text, /Engines:\n {2}Tunnel engine: Available\n {2}OpenVPN engine: Not installed/);
  assert.match(text, /Kill switch: Not supported/);
  assert.match(text, /Limits:\n {2}- No persistent kill switch/);
  assert.ok(!text.includes('{') && !text.includes('"'));
});

test('the log shows the newest entry first, with its time', () => {
  const entries = [
    { timestamp_unix: 1_000_000_000, actor: 'service', operation: 'startup', outcome: 'success' },
    { timestamp_unix: 1_000_000_060, actor: 'user', operation: 'connect', outcome: 'accepted' },
    { timestamp_unix: 1_000_000_120, actor: 'unauthorized', operation: 'agent_grant', outcome: 'denied' },
  ];
  assert.deepEqual(logRows(entries).map(e => e.operation), ['agent_grant', 'connect', 'startup']);
  assert.equal(logRows(entries, 2).length, 2);
  assert.equal(logTime(1_000_000_000, 'en-GB', 'UTC'), '09/09/2001, 01:46:40');
  const text = logText(entries, 'en-GB', 'UTC');
  assert.equal(text.split('\n')[0], '09/09/2001, 01:48:40  unauthorized  agent_grant  denied');
  assert.equal(isBadOutcome('denied'), true);
  assert.equal(isBadOutcome('failed'), true);
  assert.equal(isBadOutcome('network_cleanup_unverified'), true);
  assert.equal(isBadOutcome('accepted'), false);
  assert.equal(isBadOutcome('success'), false);
});
