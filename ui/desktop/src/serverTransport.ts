// A managed server for the admin views: their requests become typed operations (routes.ts) that
// the local service sends over its pinned connection with the sealed management key.
import type { Key, Transport, User } from '@admin/api';
import { ActiveElsewhere } from '@admin/connectHere';
import { activeElsewhere, startOutcome } from './connectNow';
import { failureText } from './failures';
import { localKeyId, request, saveDownload, ServiceError, type Connection, type Info, type Profile } from './ipc';
import { toOp, type Op } from './routes';

// The name of the server that is connected, for the question before switching to another.
async function activeName(): Promise<string> {
  try {
    const [{ profiles }, info] = await Promise.all([request<{ profiles: Profile[] }>({ type: 'profile_list' }), request<Info>({ type: 'info' })]);
    const active = profiles.find(p => p.name === info.connection.profile);
    return active?.display_name || active?.name || 'Another server';
  } catch {
    return 'Another server';
  }
}

export function serverTransport(server: string, onImported: () => void): Transport {
  const call = <T,>(op: Op) => request<T>({ type: 'server_call', server, op });
  return {
    desktop: true,
    call: (method, path, body) => call(toOp(method, path, body)),
    text: async path => {
      const answer = await call<unknown>(toOp('GET', path));
      return typeof answer === 'string' ? answer : JSON.stringify(answer);
    },
    save: async file => saveDownload(file.name, file.content, file.extension),
    // The key becomes a VPN connection on this device (it never leaves the local service). The
    // answer is the ID the service saved it under, for "Connect now".
    useHere: async (user: User, key: Key) => {
      const { profiles } = await request<{ profiles: Profile[] }>({ type: 'profile_list' });
      const id = localKeyId(user.name, key.protocol, key.id, profiles.map(p => p.name));
      const saved = await request<Partial<Profile> | null>({ type: 'access_key_import', name: id, access_key: key.access_key });
      onImported();
      return saved?.name || id;
    },
    // Starts the tunnel; it comes up (or fails) on its own and the connection status shows it. While
    // another server is connected the service says so, and that is passed on with its name.
    connect: async (profile, options) => {
      let answer: Connection;
      try {
        answer = await request<Connection>({ type: options?.switch ? 'switch' : 'connect', profile });
      } catch (e) {
        if (e instanceof ServiceError && activeElsewhere(e.code, e.message)) throw new ActiveElsewhere(await activeName());
        throw e;
      }
      const started = startOutcome(answer);
      if (started.kind === 'failed') throw new Error(failureText(started.code));
    },
  };
}
