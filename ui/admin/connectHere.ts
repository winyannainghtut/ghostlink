// "Connect now", after "Use on this device" (the desktop app): connect to the server just saved on
// this device. The desktop transport's `connect` starts it; while another server is connected the
// service refuses, and the person is asked before that one is disconnected. No imports, so the
// tests run it directly (node --experimental-strip-types).

// The transport's way to say "another server is connected": `active` is its name.
export class ActiveElsewhere extends Error {
  active: string;
  constructor(active: string) {
    super(`${active} is connected.`);
    this.active = active;
  }
}

// The question before switching: connecting to the new server disconnects the one in use.
export function switchQuestion(active: string, wanted: string): { title: string; text: string; action: string } {
  return {
    title: `Switch to ${wanted}?`,
    text: `${active} is connected now. Switching disconnects it first, then connects to ${wanted}.`,
    action: 'Switch',
  };
}

export type ConnectDeps = {
  connect: (profile: string, options?: { switch?: boolean }) => Promise<void>;
  confirm: (ask: { title: string; text: string; action: string }) => Promise<boolean>;
  toast: (message: string, tone?: 'ok' | 'bad' | 'info') => void;
};

// What came of it, for the caller: the tunnel was started, the person chose not to switch, or it
// failed (and the toast said why).
export type ConnectResult = 'started' | 'declined' | 'failed';

export async function connectHere({ connect, confirm, toast }: ConnectDeps, profile: string, label: string): Promise<ConnectResult> {
  try {
    try {
      await connect(profile);
    } catch (error) {
      if (!(error instanceof ActiveElsewhere)) throw error;
      if (!(await confirm(switchQuestion(error.active, label)))) return 'declined';
      await connect(profile, { switch: true });
    }
  } catch (error) {
    toast((error as Error).message, 'bad');
    return 'failed';
  }
  // The tunnel comes up on its own: the connection capsule at the top shows how it goes.
  toast(`Connecting to ${label}…`, 'info');
  return 'started';
}
