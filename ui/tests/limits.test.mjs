// Speed and data limits as typed in the admin views (admin/limits.ts).
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clientForm, clientFormChanged, dataLimitValue, defaultSpeedChange, defaultSpeedText, sameClientForm, speedChange, speedChoice, speedValue } from '../admin/limits.ts';

test('a client speed limit reads back as it was set', () => {
  assert.deepEqual(speedChoice(0), { mode: 'default', mb: '' });
  assert.deepEqual(speedChoice(-1), { mode: 'none', mb: '' });
  assert.deepEqual(speedChoice(2_500_000), { mode: 'custom', mb: '2.5' });
  assert.equal(speedValue({ mode: 'custom', mb: '2.5' }), 2_500_000);
  assert.ok(speedValue({ mode: 'custom', mb: '0.0005' }) instanceof Error);
});

test('an unchanged speed limit is left out of the save, even one the views cannot enter', () => {
  // 500 B/s, set with the server's CLI ("user speed NAME 0.5KB"), used to block every save.
  assert.equal(speedChange(speedChoice(500), 500), undefined);
  assert.equal(speedChange(speedChoice(0), 0), undefined);
  assert.equal(speedChange(speedChoice(-1), -1), undefined);
  assert.equal(speedChange({ mode: 'custom', mb: '2.0' }, 2_000_000), undefined);
  assert.equal(speedChange({ mode: 'none', mb: '' }, 500), -1);
  assert.equal(speedChange({ mode: 'custom', mb: '3' }, 500), 3_000_000);
  assert.ok(speedChange({ mode: 'custom', mb: '0.0004' }, 500) instanceof Error);
  assert.ok(speedChange({ mode: 'custom', mb: '' }, 0) instanceof Error);
});

test('an unchanged default speed limit is left out of the server settings', () => {
  assert.equal(defaultSpeedText(0), '');
  assert.equal(defaultSpeedChange(defaultSpeedText(500), 500), undefined);
  assert.equal(defaultSpeedChange('', 0), undefined);
  assert.equal(defaultSpeedChange('', 2_000_000), 0);
  assert.equal(defaultSpeedChange('5', 2_000_000), 5_000_000);
  assert.ok(defaultSpeedChange('0.0005', 2_000_000) instanceof Error);
  assert.ok(defaultSpeedChange('2e', 2_000_000) instanceof Error);
});

test('a data limit that is not a number is refused, not saved as no limit', () => {
  assert.equal(dataLimitValue(''), 0);
  assert.equal(dataLimitValue('50'), 50_000_000_000);
  assert.equal(dataLimitValue('0.5'), 500_000_000);
  // A number field shows "2e" but reports it as empty, with validity.badInput set.
  assert.ok(dataLimitValue('', true) instanceof Error);
  assert.ok(dataLimitValue('-5') instanceof Error);
  assert.ok(dataLimitValue('1e30') instanceof Error);
});

// The client settings form: Save is for edits only, and the form follows the server otherwise
// (code review 2026-11-25, U4).
const stored = { name: 'Maya phone', data_limit_bytes: 50_000_000_000, speed_limit_bps: 2_500_000, exempt_from_global_rules: false };

test('a client form is the client as the server has it, and is not a change', () => {
  const form = clientForm(stored);
  assert.deepEqual(form, { name: 'Maya phone', limit: '50', speed: { mode: 'custom', mb: '2.5' }, exempt: false });
  assert.equal(clientFormChanged(form, stored), false);
  // No limits at all, and a speed limit only the CLI can set (500 B/s): untouched is not a change.
  const bare = { name: 'Router', data_limit_bytes: 0, speed_limit_bps: 500, exempt_from_global_rules: true };
  assert.equal(clientFormChanged(clientForm(bare), bare), false);
});

test('each edit makes the form changed, and putting it back makes it unchanged again', () => {
  const form = clientForm(stored);
  assert.equal(clientFormChanged({ ...form, name: 'Maya laptop' }, stored), true);
  assert.equal(clientFormChanged({ ...form, name: 'Maya phone ' }, stored), false);
  assert.equal(clientFormChanged({ ...form, limit: '75' }, stored), true);
  assert.equal(clientFormChanged({ ...form, limit: '' }, stored), true);
  assert.equal(clientFormChanged({ ...form, limit: '50.0' }, stored), false);
  assert.equal(clientFormChanged({ ...form, speed: { mode: 'none', mb: '' } }, stored), true);
  assert.equal(clientFormChanged({ ...form, speed: { mode: 'custom', mb: '4' } }, stored), true);
  assert.equal(clientFormChanged({ ...form, exempt: true }, stored), true);
  // Half-typed text in the number field reads as empty: that is a change to look at, not "no change".
  assert.equal(clientFormChanged({ ...clientForm({ ...stored, data_limit_bytes: 0 }), limit: '' }, { ...stored, data_limit_bytes: 0 }, true), true);
  assert.equal(clientFormChanged({ ...form, limit: 'abc' }, stored), true);
});

test('two forms are the same only when every field is', () => {
  const form = clientForm(stored);
  assert.equal(sameClientForm(form, clientForm(stored)), true);
  assert.equal(sameClientForm(form, { ...form, name: 'x' }), false);
  assert.equal(sameClientForm(form, { ...form, speed: { mode: 'custom', mb: '2.50' } }), false);
});
