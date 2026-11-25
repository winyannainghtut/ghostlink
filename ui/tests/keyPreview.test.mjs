// What the Add access key sheet shows about a pasted key before it is added (desktop/src/keyPreview.ts),
// code review 2026-11-25, U7. Addresses are TEST-NET documentation addresses, secrets are made up.
// Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { importKind, previewKey, previewText } from '../desktop/src/keyPreview.ts';

const b64 = text => Buffer.from(text).toString('base64');
const b64url = text => Buffer.from(text).toString('base64url');

test('a Shadowsocks key shows its host and port, never its password', () => {
  const sip002 = `ss://${b64url('chacha20-ietf-poly1305:made-up-secret')}@203.0.113.10:8388/?outline=1#Home`;
  assert.equal(previewText(previewKey(sip002)), 'Shadowsocks · 203.0.113.10:8388');
  const legacy = `ss://${b64('chacha20-ietf-poly1305:made-up-secret@203.0.113.11:443')}#Office`;
  assert.equal(previewText(previewKey(legacy)), 'Shadowsocks · 203.0.113.11:443');
  const named = `ss://${b64url('aes-256-gcm:made-up-secret')}@vpn.example.com:443`;
  assert.equal(previewText(previewKey(named)), 'Shadowsocks · vpn.example.com:443');
  const v6 = `ss://${b64url('aes-256-gcm:made-up-secret')}@[2001:db8::1]:8443`;
  assert.equal(previewText(previewKey(v6)), 'Shadowsocks · [2001:db8::1]:8443');
  for (const key of [sip002, legacy, named, v6]) assert.ok(!previewText(previewKey(key)).includes('made-up-secret'));
});

test('other access keys', () => {
  assert.equal(previewText(previewKey('ssconf://keys.example.com/conf/abc123#Dynamic')), 'Outline dynamic key · keys.example.com');
  assert.equal(previewText(previewKey('vless://0b9a0f8e-0000-4000-8000-000000000000@203.0.113.12:443?security=reality&sni=example.com#Edge')), 'VLESS · 203.0.113.12:443');
  assert.equal(previewText(previewKey('vpn://AAAAA')), 'AmneziaVPN key');
});

test('a WireGuard or AmneziaWG file pasted as text shows its endpoint', () => {
  const wireguard = '[Interface]\nPrivateKey = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\nAddress = 10.8.0.2/32\n\n[Peer]\nPublicKey = BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=\nEndpoint = 203.0.113.20:51820\nAllowedIPs = 0.0.0.0/0\n';
  assert.equal(previewText(previewKey(wireguard)), 'WireGuard · 203.0.113.20:51820');
  const amnezia = wireguard.replace('Address = 10.8.0.2/32', 'Address = 10.8.0.2/32\nJc = 4\nJmin = 40\nJmax = 70\nS1 = 15\nH1 = 1234567');
  assert.equal(previewText(previewKey(amnezia)), 'AmneziaWG · 203.0.113.20:51820');
  assert.equal(previewKey(wireguard.replace('203.0.113.20:51820', '[2001:db8::2]:51820')).host, '2001:db8::2');
  assert.equal(previewKey('[Interface]\nPrivateKey = x\n').host, undefined);
});

test('how the service takes each kind', () => {
  assert.equal(importKind(previewKey('ss://abc@203.0.113.1:1')), 'key');
  assert.equal(importKind(previewKey('vless://x@203.0.113.1:1')), 'key');
  assert.equal(importKind(previewKey('[Interface]\nPrivateKey = x\n[Peer]\nEndpoint = 203.0.113.1:1')), 'file');
  assert.equal(importKind(previewKey('client\ndev tun\nproto udp\nremote 203.0.113.1 1194\n<ca>\n</ca>')), 'cli');
  assert.equal(previewKey('client\ndev tun\nremote 203.0.113.1 1194').kind, 'openvpn');
  assert.equal(importKind(previewKey('hello world')), 'unknown');
  assert.equal(importKind(previewKey('')), 'unknown');
  assert.equal(previewKey('   '), null);
});

test('damaged text is described as far as it goes, and never throws', () => {
  assert.equal(previewText(previewKey('ss://%%%')), 'Shadowsocks');
  assert.equal(previewText(previewKey('ss://@')), 'Shadowsocks');
  assert.equal(previewText(previewKey('vless://')), 'VLESS');
  assert.equal(previewKey('ssconf://').kind, 'outline-dynamic');
});
