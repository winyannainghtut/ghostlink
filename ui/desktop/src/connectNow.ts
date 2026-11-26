// "Connect now", offered after "Use on this device": what the service's answers to `connect` mean.
// The service starts the tunnel and answers at once, with the connection in its first state; the
// tunnel then comes up (or fails) on its own, and the connection status shows how it went. No
// imports, so the tests run it directly (node --experimental-strip-types).

// The service refuses to connect to a server while another one is active: it answers with the code
// "busy" and says to use switch. Other "busy" answers (a key that is in use cannot be replaced) are
// something else and are not this.
export function activeElsewhere(code: string | undefined, message: string | undefined): boolean {
  return code === 'busy' && /^Another (server|profile) is active/.test(message ?? '');
}

export type Started = { kind: 'started' } | { kind: 'failed'; code: string | null };

// How the connection the service answered with reads: it answers a start in the state `connecting`.
// `failed`, or `disconnected` (nothing was started), is a failure to say so about, with the code the
// service gave (failures.ts has the words).
export function startOutcome(connection: { status?: string; last_error?: string | null } | null | undefined): Started {
  const status = connection?.status;
  if (status === 'failed' || status === 'disconnected') return { kind: 'failed', code: connection?.last_error ?? null };
  return { kind: 'started' };
}
