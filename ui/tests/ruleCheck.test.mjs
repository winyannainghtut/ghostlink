// The line-by-line checks of the rules and split-tunneling lists (admin/ruleCheck.ts), code review
// 2026-11-25, U11. They must refuse what the servers refuse and, above all, accept what they
// accept (the names and addresses are documentation examples). The real validators are in
// core/outline-worker/internal/server/rules.go and core/vpn-engine/src/split.rs.
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { MAX_ENTRIES, checkAddresses, checkApps, checkPorts, checkSites, describe } from '../admin/ruleCheck.ts';

test('sites the servers accept are accepted, in every form they take', () => {
  for (const site of ['example.com', 'Example.COM', '*.example.com', '.example.com', 'example.com.', 'sub.example.co.uk', 'a-b.example.com', 'xn--80ak6aa92e.example', 'localhost']) {
    assert.equal(checkSites([site], 'split').problem, null, site);
    assert.equal(checkSites([site], 'rules').problem, null, site);
  }
  // Only a server's rules take an underscore (service names such as _dmarc.example.com).
  assert.equal(checkSites(['_dmarc.example.com'], 'rules').problem, null);
  assert.ok(checkSites(['_dmarc.example.com'], 'split').problem);
});

test('a site that is not a site name is named, with its line', () => {
  const checked = checkSites(['example.com', 'not a site', 'example.org'], 'rules');
  assert.equal(checked.problem.line, 2);
  assert.equal(checked.problem.entry, 'not a site');
  assert.match(describe(checked, 'site').text, /^Line 2: “not a site” is not a site name/);
  for (const bad of ['-example.com', 'example-.com', 'exa mple.com', 'example..com', 'ex@mple.com', 'https://example.com', 'example.com/path', 'é.example.com', `${'a'.repeat(64)}.com`]) {
    assert.ok(checkSites([bad], 'rules').problem, bad);
  }
});

test('an IP address among the blocked sites is sent to the addresses; split takes it as a name', () => {
  assert.match(checkSites(['203.0.113.7'], 'rules').problem.reason, /IP address: put it under blocked addresses/);
  assert.match(checkSites(['2001:db8::1'], 'rules').problem.reason, /IP address/);
  // The client's split tunneling reads "203.0.113.7" as dotted labels and takes it.
  assert.equal(checkSites(['203.0.113.7'], 'split').problem, null);
});

test('addresses and ranges', () => {
  for (const ok of ['203.0.113.7', '203.0.113.0/24', '0.0.0.0/0', '198.51.100.255/32', '2001:db8::1', '2001:db8::/32', '::1', '::', 'fe80::1:2:3:4', '1:2:3:4:5:6:7:8', '1:2:3:4:5:6:1.2.3.4', '::ffff:203.0.113.7', '203.0.113.7/']) {
    assert.equal(checkAddresses([ok]).problem, null, ok);
  }
  for (const bad of ['203.0.113', '203.0.113.256', '203.0.113.07', '203.0.113.0/33', '2001:db8::/129', 'example.com', '203.0.113.0/x', '1:2:3:4:5:6:7', '1:::2', '1::2::3', 'fe80::1%eth0', '12345::1', '203.0.113.0 /24', 'g::1']) {
    assert.ok(checkAddresses([bad]).problem, bad);
  }
  assert.match(describe(checkAddresses(['203.0.113.7', 'nonsense']), 'address').text, /^Line 2: “nonsense” is not an IP address or a range/);
});

test('ports and port ranges', () => {
  for (const ok of ['25', '6881-6889', '1', '65535', '80 - 90', '443-443']) assert.equal(checkPorts([ok]).problem, null, ok);
  for (const bad of ['0', '65536', 'http', '6889-6881', '25-', '-25', '25,26', '1.5', '25 26', '0-10']) assert.ok(checkPorts([bad]).problem, bad);
});

test('apps are names or paths, without control characters', () => {
  for (const ok of ['chrome.exe', 'C:\\Program Files\\App\\app.exe', '/usr/bin/firefox', 'Chrome']) assert.equal(checkApps([ok]).problem, null, ok);
  assert.ok(checkApps(['bad\u0007name']).problem);
  assert.ok(checkApps(['x'.repeat(261)]).problem);
  assert.equal(checkApps(['x'.repeat(260)]).problem, null);
});

test('a list over the limit says so, and an empty one is fine', () => {
  const many = Array.from({ length: MAX_ENTRIES + 1 }, (_, i) => `site${i}.example.com`);
  assert.match(describe(checkSites(many, 'rules'), 'site').text, /At most 512 sites/);
  assert.equal(checkSites(many.slice(0, MAX_ENTRIES), 'rules').problem, null);
  assert.deepEqual(describe(checkSites([], 'rules'), 'site'), { ok: true, text: '' });
  assert.deepEqual(describe(checkSites(['example.com'], 'rules'), 'site'), { ok: true, text: '1 site' });
  assert.deepEqual(describe(checkPorts(['25', '80']), 'port'), { ok: true, text: '2 ports' });
  assert.deepEqual(describe(checkAddresses(['203.0.113.7', '198.51.100.0/24']), 'address', 'addresses'), { ok: true, text: '2 addresses' });
});
