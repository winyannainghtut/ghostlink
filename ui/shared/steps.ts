// A flow whose later steps need what the earlier ones made, such as "create the client, then its
// key". The results of the steps that worked are kept in `done`, so running the flow again after a
// failure resumes at the step that failed. Without that, a second try would repeat the first step:
// create the client again (and fail, the name is taken) or save the same server again under a new
// ID. No imports, so the tests run it directly (node --experimental-strip-types).
export type Done = Map<string, unknown>;

export async function step<T>(done: Done, name: string, work: () => Promise<T>): Promise<T> {
  if (done.has(name)) return done.get(name) as T;
  const value = await work();
  done.set(name, value);
  return value;
}
