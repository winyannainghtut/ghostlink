// A file dropped on Add access key (desktop/src/dropFile.ts): what is accepted, how it is handled
// like pasted text, and that the window lets the sheet have the drop. Run:
// node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { droppedText, guardFileDrops, hasFiles, MAX_FILE_BYTES, nextDepth, OPENVPN_NOTE, pickDropped } from '../desktop/src/dropFile.ts';
import { importKind, previewKey } from '../desktop/src/keyPreview.ts';

const WIREGUARD = ['[Interface]', 'PrivateKey = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=', 'Address = 10.8.0.2/32', '', '[Peer]', 'PublicKey = BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=', 'Endpoint = 203.0.113.10:51820', 'AllowedIPs = 0.0.0.0/0', ''].join('\n');

test('the window gives the page the file drops (Tauri takes them first unless it is told not to)', () => {
  const config = JSON.parse(readFileSync(new URL('../../apps/desktop/src-tauri/tauri.conf.json', import.meta.url), 'utf8'));
  const windows = config.app.windows;
  assert.ok(windows.length > 0);
  for (const window of windows) assert.equal(window.dragDropEnabled, false, 'dragDropEnabled must be false for the page to see a dropped file');
});

test('only a drag that carries files is the zone\'s', () => {
  assert.equal(hasFiles(['Files']), true);
  assert.equal(hasFiles(['text/plain', 'Files']), true);
  // A selection of text or a dragged link is left to the page.
  assert.equal(hasFiles(['text/plain']), false);
  assert.equal(hasFiles(['text/uri-list', 'text/html']), false);
  assert.equal(hasFiles([]), false);
  assert.equal(hasFiles(null), false);
  assert.equal(hasFiles(undefined), false);
});

test('the zone counts the elements the pointer crosses, and is left only when it is all the way out', () => {
  // Over the sheet, then onto a child (the sheet's leave comes after the child's enter), then out.
  let depth = 0;
  depth = nextDepth(depth, 'enter');
  assert.equal(depth, 1);
  depth = nextDepth(depth, 'enter');
  depth = nextDepth(depth, 'leave');
  assert.equal(depth, 1, 'still over the zone');
  depth = nextDepth(depth, 'leave');
  assert.equal(depth, 0);
  assert.equal(nextDepth(0, 'leave'), 0, 'never below zero');
  assert.equal(nextDepth(3, 'drop'), 0);
});

test('one file is read; none, several and OpenVPN profiles are answered in words', () => {
  const conf = { name: 'singapore.conf', size: 300 };
  assert.deepEqual(pickDropped([conf]), { file: conf });
  assert.match(pickDropped([]).error, /Nothing was dropped/);
  assert.match(pickDropped([conf, conf]).error, /one file at a time/);
  assert.equal(pickDropped([{ name: 'office.OVPN', size: 4000 }]).error, OPENVPN_NOTE);
  // An OpenVPN profile over the cap gets the OpenVPN answer, not "too large".
  assert.equal(pickDropped([{ name: 'big.ovpn', size: MAX_FILE_BYTES * 4 }]).error, OPENVPN_NOTE);
  assert.match(pickDropped([{ name: 'movie.conf', size: MAX_FILE_BYTES + 1 }]).error, /too large/);
  assert.ok(pickDropped([{ name: 'edge.conf', size: MAX_FILE_BYTES }]).file, 'exactly the cap is read');
});

test('a file that is not text is refused', () => {
  assert.match(droppedText('PK\u0003\u0004\u0000\u0000binary').error, /not a text file/);
  // What a text read makes of bytes that are not UTF-8.
  assert.match(droppedText(String.fromCharCode(0xfffd).repeat(3)).error, /not a text file/);
  assert.match(droppedText('   \n ').error, /empty/);
  assert.match(droppedText('x'.repeat(MAX_FILE_BYTES + 1)).error, /too large/);
  assert.deepEqual(droppedText(WIREGUARD), { text: WIREGUARD });
});

test('a dropped file ends up where the same text pasted would', () => {
  // The sheet puts the text in its box and previews it with previewKey: what the text is decides how
  // it is added (a file goes to profile_import, a key to access_key_import, OpenVPN to the note).
  const route = text => importKind(previewKey(droppedText(text).text));
  assert.equal(route(WIREGUARD), 'file');
  assert.equal(route(WIREGUARD.replace('[Interface]', '[Interface]\nJc = 4')), 'file');
  assert.equal(route('ss://Y2hhY2hhMjAtaWV0Zi1wb2x5MTMwNTpwYXNz@203.0.113.10:443/?outline=1#Home\n'), 'key');
  assert.equal(route('vless://11111111-2222-3333-4444-555555555555@203.0.113.10:443?security=reality#x'), 'key');
  assert.equal(route('client\ndev tun\nproto udp\nremote 203.0.113.10 1194\n'), 'cli');
  assert.equal(route('Dear Maya, here is the file.'), 'unknown');
  assert.equal(route(WIREGUARD), importKind(previewKey(WIREGUARD)), 'the same as pasting');
});

// A stand-in for the window: records listeners, and fires events at them.
function windowWithGuard() {
  const listeners = {};
  guardFileDrops({ addEventListener: (type, listener) => (listeners[type] = listener) });
  const event = (types, taken = false) => {
    const e = { dataTransfer: { types, dropEffect: 'copy' }, defaultPrevented: taken, prevented: false };
    e.preventDefault = () => ((e.prevented = true), (e.defaultPrevented = true));
    return e;
  };
  return { listeners, event };
}

test('a file dropped outside the sheet is stopped from opening in the window', () => {
  const { listeners, event } = windowWithGuard();
  assert.deepEqual(Object.keys(listeners).sort(), ['dragover', 'drop']);
  const over = event(['Files']);
  listeners.dragover(over);
  assert.equal(over.prevented, true);
  assert.equal(over.dataTransfer.dropEffect, 'none', 'the pointer says it cannot be dropped here');
  const drop = event(['Files']);
  listeners.drop(drop);
  assert.equal(drop.prevented, true, 'without this the window would navigate to the file');
});

test('a drop the sheet took keeps its cursor, and text drags are left alone', () => {
  const { listeners, event } = windowWithGuard();
  const taken = event(['Files'], true);
  listeners.dragover(taken);
  assert.equal(taken.dataTransfer.dropEffect, 'copy');
  const text = event(['text/plain']);
  listeners.dragover(text);
  listeners.drop(text);
  assert.equal(text.prevented, false);
  listeners.drop({ dataTransfer: null, defaultPrevented: false, preventDefault: () => assert.fail('no data') });
});
