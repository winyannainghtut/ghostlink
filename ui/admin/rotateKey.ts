// Replacing the management key, from the server's settings: what the person is told before, and
// what the answer means after. The desktop app's service rotates the key on the server and seals
// the new one on this device (its answer is `{rotated: true, server}`: the key is never returned);
// the web dashboard calls the server itself, which answers with the new key
// (`{management_key: {apiUrl, certSha256}}`). No imports, so the tests run it directly
// (node --experimental-strip-types).

export const ROTATE_TITLE = 'Replace the management key?';

// The warning, in the three places it is read: the confirmation in the app, the confirmation in the
// dashboard, and under the new key once the dashboard shows it. It says what happens to the old key
// and where the new one goes.
export type WarningFor = 'app' | 'dashboard' | 'shown';
export function rotateWarning(where: WarningFor): string {
  const old = where === 'shown' ? 'The old management key no longer works, on any device or script that has it.' : 'The old management key stops working at once, on every device and script that has it.';
  const next =
    where === 'app'
      ? 'The new key is saved on this device.'
      : where === 'dashboard'
        ? 'The new key is shown once, on the next screen: this dashboard does not keep it.'
        : 'This is the only time the dashboard shows the new key, and it does not keep it.';
  return `${old} ${next} Other devices need the new key: sudo ghostlink-server show-key prints it on the server. Clients stay connected.`;
}

export type Rotation =
  // The app: the new key is on this device, and the service keeps it.
  | { kind: 'saved'; message: string }
  // The dashboard: the new key as the text the app and the command line take, to show once.
  | { kind: 'shown'; key: string; message: string }
  // An answer that is neither: the server may have changed its key, so say how to get it.
  | { kind: 'unclear'; message: string };

const text = (value: unknown): value is string => typeof value === 'string' && value.length > 0;

export function rotationResult(answer: unknown): Rotation {
  const body = (answer && typeof answer === 'object' ? answer : {}) as Record<string, unknown>;
  if (body.rotated === true) {
    return { kind: 'saved', message: 'The new key is saved on this device. Other devices need the new key: sudo ghostlink-server show-key prints it on the server.' };
  }
  const key = body.management_key as Record<string, unknown> | undefined;
  if (key && text(key.apiUrl) && text(key.certSha256)) {
    // The same two fields, in the same order, as `ghostlink-server show-key` prints them.
    return { kind: 'shown', key: JSON.stringify({ apiUrl: key.apiUrl, certSha256: key.certSha256 }), message: 'The management key is replaced.' };
  }
  return { kind: 'unclear', message: 'The server answered, but not with a new key. The key may have been replaced: sudo ghostlink-server show-key prints the one that works.' };
}
