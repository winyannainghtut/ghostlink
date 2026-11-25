// The login token in what a person pasted into the sign-in card: the whole login link
// ("https://host:8443/#t=TOKEN", as `sudo ghostlink-server dashboard` prints it), its part after
// the #, "t=TOKEN", or the token alone. The server's tokens are 22 URL-safe base64 characters. No
// imports, so the tests run it directly (node --experimental-strip-types).
export function parseLoginLink(text: string): string | null {
  const value = text.trim();
  if (!value) return null;
  const hash = value.indexOf('#');
  const fromParams = (params: string) => new URLSearchParams(params).get('t');
  const token = hash >= 0 ? fromParams(value.slice(hash + 1)) : /^t=/.test(value) ? fromParams(value) : value;
  return token && /^[A-Za-z0-9_-]{16,128}$/.test(token) ? token : null;
}
