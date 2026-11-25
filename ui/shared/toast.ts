// How long a toast stays: long enough to be read. A short "Key copied." keeps its 3.2 s (6 s for an
// error); a notice of several sentences, such as the firewall ports to allow after turning a protocol
// on, gets about 55 ms a character after a 2 s start, up to 20 s. No imports, so the tests run it
// directly (node --experimental-strip-types).
export function toastDuration(message: string, tone: 'ok' | 'bad' | 'info'): number {
  const base = tone === 'bad' ? 6000 : 3200;
  return Math.min(20000, Math.max(base, 2000 + message.length * 55));
}

// What is left of a toast's time when the pointer or the keyboard leaves it after a pause: never
// less than 1.5 s, so a toast does not vanish the moment it is let go.
export function timeLeft(total: number, shown: number): number {
  return Math.max(1500, total - shown);
}
