// One run at a time for a button that changes something (a save, a create). The lock is a plain
// object, not React state, so a second click in the same instant, before the page has drawn the
// disabled button, is refused too. No imports, so the tests run it directly.
export type Lock = { running: boolean };

// Runs `work` unless it is already running; undefined means it was refused. The lock is released
// when the work ends, however it ends.
export async function exclusive<T>(lock: Lock, work: () => Promise<T>): Promise<T | undefined> {
  if (lock.running) return undefined;
  lock.running = true;
  try {
    return await work();
  } finally {
    lock.running = false;
  }
}
