// What a pasted key or file is, before it is added: its kind, and the server's host and port when the
// text says them, so the sheet can show "Shadowsocks · 203.0.113.10:443" and the person can see it is
// the right key. Only the kind, host and port are read: a password or key in the text is never put
// on screen. The service does the real parsing and validation when the key is added; this only
// looks. No imports, so the tests run it directly (node --experimental-strip-types).

export type KeyKind = 'shadowsocks' | 'outline-dynamic' | 'vless' | 'amnezia' | 'wireguard' | 'amneziawg' | 'openvpn' | 'unknown';
export type KeyPreview = { kind: KeyKind; name: string; host?: string; port?: number };

const NAMES: Record<KeyKind, string> = {
  shadowsocks: 'Shadowsocks',
  'outline-dynamic': 'Outline dynamic key',
  vless: 'VLESS',
  amnezia: 'AmneziaVPN key',
  wireguard: 'WireGuard',
  amneziawg: 'AmneziaWG',
  openvpn: 'OpenVPN',
  unknown: 'Unknown',
};

// "203.0.113.10:443", "[2001:db8::1]:443" or "vpn.example.com": host and, when there is one, port.
function hostPort(text: string): { host: string; port?: number } | null {
  const value = text.trim();
  const v6 = /^\[([0-9a-f:.]+)\](?::(\d{1,5}))?$/i.exec(value);
  if (v6) return { host: v6[1], port: v6[2] ? Number(v6[2]) : undefined };
  const m = /^([^\s:/?#@[\]]+)(?::(\d{1,5}))?$/.exec(value);
  if (!m) return null;
  return { host: m[1], port: m[2] ? Number(m[2]) : undefined };
}

// atob without the padding rules: keys are base64 or base64url, with or without "=".
function decode(text: string): string | null {
  try {
    const clean = text.replace(/-/g, '+').replace(/_/g, '/').replace(/=+$/, '');
    return atob(clean + '='.repeat((4 - (clean.length % 4)) % 4));
  } catch {
    return null;
  }
}

function make(kind: KeyKind, address: { host: string; port?: number } | null): KeyPreview {
  return { kind, name: NAMES[kind], ...(address ?? {}) };
}

export function previewKey(text: string): KeyPreview | null {
  const source = text.trim();
  if (!source) return null;

  if (source.startsWith('ss://')) {
    // SIP002: ss://BASE64(method:password)@host:port/?options#label. The older form puts everything
    // in the base64: ss://BASE64(method:password@host:port)#label.
    const body = source.slice('ss://'.length).split('#')[0];
    const at = body.lastIndexOf('@');
    if (at >= 0) return make('shadowsocks', hostPort(body.slice(at + 1).split(/[/?]/)[0]));
    const inner = decode(body.split(/[/?]/)[0]);
    const innerAt = inner ? inner.lastIndexOf('@') : -1;
    return make('shadowsocks', inner && innerAt >= 0 ? hostPort(inner.slice(innerAt + 1)) : null);
  }
  if (source.startsWith('ssconf://')) return make('outline-dynamic', hostPort(source.slice('ssconf://'.length).split(/[/?#]/)[0]));
  if (source.startsWith('vless://')) {
    const body = source.slice('vless://'.length).split(/[?#]/)[0];
    const at = body.lastIndexOf('@');
    return make('vless', hostPort((at >= 0 ? body.slice(at + 1) : body).split('/')[0]));
  }
  // The server inside an Amnezia key is packed (compressed JSON): only the service reads it.
  if (source.startsWith('vpn://')) return make('amnezia', null);

  if (/^\s*\[interface\]/im.test(source) || (/^\s*privatekey\s*=/im.test(source) && /^\s*\[peer\]/im.test(source))) {
    const endpoint = /^\s*endpoint\s*=\s*(\S+)/im.exec(source);
    // AmneziaWG is WireGuard with obfuscation settings in [Interface].
    const amnezia = /^\s*(jc|jmin|jmax|s[1-4]|h[1-4]|i[1-5])\s*=/im.test(source);
    return make(amnezia ? 'amneziawg' : 'wireguard', endpoint ? hostPort(endpoint[1]) : null);
  }
  if (/^\s*(client|remote\s+\S+|dev\s+(tun|tap)|proto\s+(udp|tcp))\b/im.test(source) || /<ca>/i.test(source)) return make('openvpn', null);
  return make('unknown', null);
}

// How the preview reads under the field: "Shadowsocks · 203.0.113.10:443".
export function previewText(preview: KeyPreview): string {
  const address = preview.host ? `${preview.host.includes(':') ? `[${preview.host}]` : preview.host}${preview.port ? `:${preview.port}` : ''}` : '';
  return address ? `${preview.name} · ${address}` : preview.name;
}

// How the service takes it: an access key (ss://, ssconf://, vless://, vpn://), a WireGuard or
// AmneziaWG file, or neither (OpenVPN profiles are imported with the command line).
export function importKind(preview: KeyPreview | null): 'key' | 'file' | 'cli' | 'unknown' {
  if (!preview) return 'unknown';
  if (preview.kind === 'wireguard' || preview.kind === 'amneziawg') return 'file';
  if (preview.kind === 'openvpn') return 'cli';
  return preview.kind === 'unknown' ? 'unknown' : 'key';
}
