// React render tests (code review 2026-11-25, U14): components are rendered to HTML on the server
// side of react-dom, with Vite loading their .tsx (tests/support/render.mjs), so what a view first
// paints is checked without a browser. Effects do not run, so the views are as they are before
// their first answer arrives. Run: node --experimental-strip-types --test tests/
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { after, before, test } from 'node:test';
import { createElement as h, Fragment } from 'react';
import { startRenderer } from './support/render.mjs';

let ui;
before(async () => {
  ui = await startRenderer();
});
after(() => ui?.close());

// The server a view is shown for (invented values, TEST-NET address).
const server = (extra = {}) => ({
  name: 'Home A',
  hostname: '203.0.113.10',
  version: '0.9.2',
  created_unix: 1700000000,
  api_port: 8443,
  protocols: { shadowsocks: { enabled: true, port: 443 }, wireguard: { enabled: false, port: 51820 }, amneziawg: { enabled: false, port: 51821 }, vless: { enabled: false, port: 8444 }, ikev2: { enabled: false, port: 500 }, ipsec: { enabled: false, port: 1701 } },
  vless_handshake: 'www.example.com:443',
  shadowsocks_method: 'chacha20-ietf-poly1305',
  default_speed_limit_bps: 0,
  users: 0,
  site_stats: false,
  history_days: 30,
  site_days: 7,
  history_max_mb: 50,
  dashboard_port: 8080,
  dashboard_public: false,
  ...extra,
});
const provider = { enabled: false, ready: false, updating: false, ranges: 0, source: 'iptoasn.com' };
const transport = { call: async () => ({}), text: async () => '', save: async () => null };

// A view inside the real store provider (nothing is polled in a server render).
async function inStore(path, name, props, serverValue = server(), transportValue = transport) {
  const { StoreProvider } = await ui.load('/admin/store.tsx');
  const view = (await ui.load(path))[name];
  return ui.render(h(StoreProvider, { transport: transportValue, initial: serverValue, onSignedOut() {} }, h(view, props)));
}

test('Clients shows placeholders, never "No matching clients", until the first answer', async () => {
  const html = await inStore('/admin/views/Clients.tsx', 'Clients', { onNewClient() {} });
  assert.match(html, /role="status"[^>]*aria-busy="true"/);
  assert.match(html, /Loading clients/);
  assert.ok((html.match(/animate-pulse/g) ?? []).length >= 6, 'skeleton shapes of the finished cards');
  assert.doesNotMatch(html, /No matching clients/);
  assert.doesNotMatch(html, /No clients yet/);
  // The search and filters are there at once; the counts wait for the answer.
  assert.match(html, /aria-label="Search clients"/);
});

test('Bars: placeholders while loading, the empty text only for nothing, "+N more" past eight', async () => {
  const { Bars } = await ui.load('/shared/Bars.tsx');
  const items = n => Array.from({ length: n }, (_, i) => ({ label: `Service ${i + 1}`, bytes: (n - i) * 1000 }));
  const loading = ui.render(h(Bars, { items: undefined, empty: 'Nothing recorded yet' }));
  assert.match(loading, /aria-busy="true"/);
  assert.doesNotMatch(loading, /Nothing recorded yet/);
  for (const nothing of [null, []]) assert.match(ui.render(h(Bars, { items: nothing, empty: 'Nothing recorded yet' })), /Nothing recorded yet/);

  const eleven = ui.render(h(Bars, { items: items(11), empty: 'x' }));
  assert.equal((eleven.match(/<li/g) ?? []).length, 8, 'only the first eight rows');
  assert.match(eleven, /\+3 more/);
  assert.match(eleven, /aria-expanded="false"/);
  assert.doesNotMatch(eleven, /Service 9\b/);

  const eight = ui.render(h(Bars, { items: items(8), empty: 'x' }));
  assert.equal((eight.match(/<li/g) ?? []).length, 8);
  assert.doesNotMatch(eight, /more/, 'eight rows need no button');
  assert.match(ui.render(h(Bars, { items: items(5), empty: 'x', limit: 3 })), /\+2 more/);
});

test('AreaChart keeps a gutter for its axis labels, and two charts do not share gradient ids', async () => {
  const { AreaChart } = await ui.load('/shared/AreaChart.tsx');
  const points = Array.from({ length: 30 }, (_, i) => ({ t: 1700000000 + i * 60, down: 400_000 + i * 20_000, up: 80_000 }));
  const chart = () => h(AreaChart, { points, timeLabel: t => String(t) });
  const html = ui.render(h(Fragment, null, chart(), chart()));

  const ids = [...html.matchAll(/<linearGradient id="([^"]+)"/g)].map(m => m[1]);
  assert.equal(ids.length, 4, 'two gradients in each of two charts');
  assert.equal(new Set(ids).size, 4, `gradient ids repeat: ${ids}`);
  for (const id of ids) assert.match(id, /^[A-Za-z0-9_-]+$/, 'an id a url(#...) reference is safe with');
  // Every fill points at a gradient of the page, and the two charts point at different ones.
  const fills = [...html.matchAll(/fill="url\(#([^)]+)\)"/g)].map(m => m[1]);
  assert.equal(fills.length, 4);
  for (const id of fills) assert.ok(ids.includes(id), `${id} is not defined`);
  assert.equal(new Set(fills).size, 4);

  // The gutter: the grid lines end before the tick labels begin. The labels are right-aligned at
  // width - 4; the widest takes about 5.3 px per character at 10 px.
  const svg = html.slice(html.indexOf('<svg'), html.indexOf('</svg>'));
  const width = Number(/width="(\d+)"/.exec(svg)[1]);
  const labels = [...svg.matchAll(/<text class="axis-label" x="([\d.]+)" y="[\d.]+" text-anchor="end" dominant-baseline="central">([^<]+)</g)];
  assert.equal(labels.length, 4, 'four scale labels');
  const widest = Math.max(...labels.map(m => m[2].length)) * 5.3;
  const gridEnds = [...svg.matchAll(/<line class="gridline" x1="[\d.]+" x2="([\d.]+)"/g)].map(m => Number(m[1]));
  assert.ok(gridEnds.length >= 4);
  for (const end of gridEnds) assert.ok(end <= width - 4 - widest, `a grid line runs to ${end}, into the labels (they start at ${width - 4 - widest})`);
  assert.match(svg, /No\b|aria-label="Throughput over time"/);
});

test('the server settings offer the provider switch and replacing the management key', async () => {
  const html = await inStore('/admin/views/Settings.tsx', 'SettingsSheet', { open: true, onClose() {} }, server({ asn: provider }));
  assert.match(html, /Server settings/);
  assert.match(html, /Show the provider \(AS and operator\) of each client address/);
  assert.match(html, /role="switch"/);
  assert.match(html, /Management key/);
  assert.match(html, /Replace the management key/);
  // The warning on the page says what the confirmation will say.
  assert.match(html, /stops working at once, on every device and script that has it/);
  // The group key of IPsec is for servers that have IPsec on only.
  assert.doesNotMatch(html, /Replace the group key/);
  const withIpsec = await inStore('/admin/views/Settings.tsx', 'SettingsSheet', { open: true, onClose() {} }, server({ asn: provider, protocols: { ...server().protocols, ipsec: { enabled: true, port: 1701 } } }));
  assert.match(withIpsec, /Replace the group key/);
  assert.match(withIpsec, /Replace the management key/);
});

test('a server before 0.9.0 has no provider switch, and can still replace its key', async () => {
  const html = await inStore('/admin/views/Settings.tsx', 'SettingsSheet', { open: true, onClose() {} }, server());
  assert.doesNotMatch(html, /Show the provider/);
  assert.match(html, /Replace the management key/);
});

test('the new management key is not on the page until the dashboard shows it', async () => {
  const html = await inStore('/admin/views/Settings.tsx', 'SettingsSheet', { open: true, onClose() {} }, server());
  assert.doesNotMatch(html, /The new management key/);
  assert.doesNotMatch(html, /apiUrl/);
});

test('Add access key takes a pasted key, a chosen file and a dropped one, and shows the zone only during a drag', async () => {
  const { ServiceProvider } = await ui.load('/desktop/src/service.tsx');
  const { AddKeySheet } = await ui.load('/desktop/src/views/AddKeySheet.tsx');
  const html = ui.render(h(ServiceProvider, null, h(AddKeySheet, { open: true, onClose() {}, replacing: null })));
  assert.match(html, /Add access key/);
  assert.match(html, />Paste</);
  assert.match(html, /Import a \.conf file/);
  assert.match(html, /drop the file on this window/);
  assert.doesNotMatch(html, /data-drop-zone/, 'no zone until a file is dragged over the sheet');
  assert.doesNotMatch(html, /Drop the file to read it/);
  // Closed, the sheet has nothing in it.
  assert.doesNotMatch(ui.render(h(ServiceProvider, null, h(AddKeySheet, { open: false, onClose() {}, replacing: null }))), /Paste/);
});

test('the zoom controls and the focusable map are there, and the map starts unzoomed', async () => {
  const { MeshMap } = await ui.load('/shared/MeshMap.tsx');
  const topology = {
    id: 'mesh-1',
    name: 'Test mesh',
    cidr: '100.96.0.0/16',
    enabled: true,
    rule_count: 0,
    updated_unix: 1700000000,
    nodes: Array.from({ length: 12 }, (_, i) => ({ id: String(i + 1), name: `node-${i + 1}`, address: `100.96.0.${i + 1}`, tags: null, status: 'active', relay: i === 0, relay_in_use: false, online: true, last_seen_unix: 1700000000, endpoint: '', seen_from: '', listen_port: 51820, advertised_routes: null, approved_routes: null, masquerade: false, version: '0.9.2', os: 'linux', in_sync: true, rx_bps: 0, tx_bps: 0 })),
    links: [],
  };
  const html = ui.render(h(MeshMap, { topology, selected: null, onSelect() {} }));
  for (const label of ['Zoom out', 'Zoom in', 'Show the whole map']) assert.match(html, new RegExp(`aria-label="${label}"`));
  assert.match(html, /100(<!-- -->)?%/);
  assert.match(html, /tabindex="0"/);
  assert.match(html, /translate\(0\.00 0\.00\) scale\(1\.0000\)/);
  // No nodes yet: the map says so, and has no controls to offer.
  const empty = ui.render(h(MeshMap, { topology: { ...topology, nodes: [] }, selected: null, onSelect() {} }));
  assert.match(empty, /No nodes yet/);
  assert.doesNotMatch(empty, /Zoom in/);
});

test('warning text uses its own colour, and in Daylight it reads on a light card', async () => {
  const { Note } = await ui.load('/shared/glass.tsx');
  const warn = ui.render(h(Note, { tone: 'warn' }, 'The disk is nearly full.'));
  assert.match(warn, /text-warn/);
  assert.doesNotMatch(warn, /text-idle/, 'the status colour is for dots, not for text');

  const css = readFileSync(new URL('../shared/styles.css', import.meta.url), 'utf8');
  const block = name => css.slice(css.indexOf(name), css.indexOf('}', css.indexOf(name)));
  const hex = block("[data-finish='daylight']").match(/--warn:\s*(#[0-9a-f]{6})/i)?.[1];
  assert.ok(hex, 'Daylight sets its own --warn');
  const luminance = color => {
    const [r, g, b] = [1, 3, 5].map(i => parseInt(color.slice(i, i + 2), 16) / 255).map(c => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  // The darkest pixel behind a card's text in Daylight, sampled in the app with the text hidden (the
  // cards over a wave crest, 2026-11-26): relative luminance 0.70. The page's own colour is lighter.
  const ratio = (color, backgroundLuminance) => (Math.max(luminance(color), backgroundLuminance) + 0.05) / (Math.min(luminance(color), backgroundLuminance) + 0.05);
  assert.ok(ratio(hex, 0.7) >= 4.5, `${hex} on the darkest card pixel is ${ratio(hex, 0.7).toFixed(2)}:1`);
  assert.ok(ratio(hex, luminance('#f2f4f8')) >= 4.5);
  // The yellow it replaced as text colour: 1.75:1 on that pixel, which is why it was not readable.
  const old = block("[data-finish='daylight']").match(/--idle:\s*(#[0-9a-f]{6})/i)?.[1];
  assert.ok(ratio(old, 0.7) < 3, `the status colour ${old} would be ${ratio(old, 0.7).toFixed(2)}:1`);
  // The status colour is not the text colour: the dots keep theirs.
  assert.notEqual(block("[data-finish='daylight']").match(/--idle:\s*(#[0-9a-f]{6})/i)?.[1], hex);
});
