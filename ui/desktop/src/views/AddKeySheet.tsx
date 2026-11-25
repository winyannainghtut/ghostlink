// Add an access key to this device, or replace the key of a server already here. The key can be an
// ss://, ssconf://, vless:// or vpn:// key, or the text of a WireGuard or AmneziaWG file (pasted or
// from a file): the sheet shows what it is (protocol, host, port) before it is added.
import { ClipboardPaste, Eye, EyeOff, FileUp } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { Button, Field, IconButton, Note, Sheet, useUI } from '@shared/glass';
import { useBusy } from '@shared/hooks';
import { step, type Done } from '@shared/steps';
import { request, slug, uniqueName, type Profile } from '../ipc';
import { importKind, previewKey, previewText } from '../keyPreview';
import { useService } from '../service';

export function AddKeySheet({ open, onClose, replacing }: { open: boolean; onClose: () => void; replacing: Profile | null }) {
  const { profiles, refresh } = useService();
  const { toast } = useUI();
  const [name, setName] = useState('');
  const [key, setKey] = useState('');
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState('');
  const { busy, run } = useBusy();
  const file = useRef<HTMLInputElement>(null);
  // Saving the key and naming it are two requests. When the name fails the key is saved already:
  // the steps that worked are kept, and the next try only names it (a second import would save the
  // same server again under another ID).
  const done = useRef<Done>(new Map());
  useEffect(() => {
    if (!open) {
      setKey('');
      setReveal(false);
      return;
    }
    done.current = new Map();
    setName(replacing ? replacing.display_name || replacing.name : '');
    setError('');
  }, [open, replacing]);
  const taken = profiles.map(p => p.name);
  const preview = previewKey(key);
  const kind = importKind(preview);

  // The key is in the list already after its first step: show it, also when the sheet is closed.
  const close = () => {
    if (done.current.has('save')) refresh().catch(() => {});
    onClose();
  };

  const submit = (source: string, as: 'key' | 'file', fallbackName: string) =>
    run(async () => {
      setError('');
      const displayName = name.trim() || fallbackName;
      try {
        const id = await step(done.current, 'save', async () => {
          const id = replacing ? replacing.name : uniqueName(slug(displayName || 'server'), taken);
          const replace = replacing ? { replace: true } : {};
          if (as === 'key') await request({ type: 'access_key_import', name: id, access_key: source, ...replace });
          else await request({ type: 'profile_import', name: id, content: source, ...replace });
          return id;
        });
        if (displayName) await request({ type: 'profile_rename', name: id, display_name: displayName.slice(0, 80) });
        setKey('');
        await refresh();
        toast(replacing ? `${displayName || id} has its new key.` : `${displayName || id} added.`);
        onClose();
      } catch (e) {
        const message = (e as Error).message;
        setError(done.current.has('save') ? `The key is saved, but its name could not be set: ${message} Try again, or close this window: the server is in your list.` : message);
      }
    });

  const add = () => {
    const source = key.trim();
    if (kind === 'cli') {
      setError('This is an OpenVPN profile. The app adds access keys and WireGuard or AmneziaWG files; import an OpenVPN profile with the command line.');
      return;
    }
    if (kind === 'unknown') {
      setError('Paste a key beginning with ss://, ssconf://, vless:// or vpn://, or the text of a WireGuard or AmneziaWG file.');
      return;
    }
    return submit(source, kind === 'file' ? 'file' : 'key', kind === 'file' ? preview?.host || 'WireGuard' : '');
  };

  // WireGuard/AmneziaWG files are read here and sent straight to the service.
  const importFile = async (f: File) => {
    setError('');
    if (f.size > 65536) {
      setError('That file is too large to be a WireGuard configuration.');
      return;
    }
    const text = await f.text();
    const fileKind = importKind(previewKey(text));
    if (fileKind === 'cli') {
      setError('This is an OpenVPN profile. The app adds access keys and WireGuard or AmneziaWG files; import an OpenVPN profile with the command line.');
      return;
    }
    await submit(text, 'file', f.name.replace(/\.(conf|txt)$/i, ''));
  };

  const paste = async () => {
    try {
      setKey(await navigator.clipboard.readText());
      setError('');
    } catch {
      toast('Could not read the clipboard. Paste into the box with Ctrl+V instead.', 'info');
    }
  };

  return (
    <Sheet
      open={open}
      onClose={close}
      title={replacing ? `Replace the key of ${replacing.display_name || replacing.name}` : 'Add access key'}
      subtitle={replacing ? 'The server keeps its name, its place in the list and its split-tunneling settings. Disconnect from it first.' : 'Paste the key shared by your server administrator: Outline, VLESS or Amnezia, or the text of a WireGuard file.'}
      footer={
        <>
          <Button variant="ghost" icon={<FileUp size={15} />} disabled={busy} onClick={() => file.current?.click()}>
            Import a .conf file
          </Button>
          <Button variant="cta" disabled={busy || !key.trim()} onClick={add}>
            {busy ? (replacing ? 'Replacing…' : 'Adding…') : done.current.has('save') ? 'Set the name' : replacing ? 'Replace key' : 'Add server'}
          </Button>
        </>
      }
    >
      <form
        className="grid gap-4"
        onSubmit={e => {
          e.preventDefault();
          add();
        }}
      >
        <Field label="Name on this device">
          <input className="field" maxLength={80} value={name} onChange={e => setName(e.target.value)} placeholder="For example Singapore" autoComplete="off" data-autofocus />
        </Field>
        <div className="grid gap-1.5">
          <div className="flex items-center justify-between gap-2">
            <label htmlFor="add-key-text" className="text-[12.5px] font-medium text-fg-2">
              Access key or WireGuard file
            </label>
            <Button size="sm" variant="ghost" icon={<ClipboardPaste size={14} />} disabled={busy} onClick={paste}>
              Paste
            </Button>
          </div>
          <div className="relative">
            <textarea
              id="add-key-text"
              className={`field mono min-h-[96px] pr-12 ${reveal ? '' : 'masked'}`}
              value={key}
              onChange={e => setKey(e.target.value)}
              placeholder={'ss://, ssconf://, vless:// or vpn://\nor the text of a WireGuard or AmneziaWG .conf file'}
              autoComplete="off"
              spellCheck={false}
            />
            <IconButton label={reveal ? 'Hide key' : 'Show key'} className="absolute top-1 right-1" onClick={() => setReveal(!reveal)}>
              {reveal ? <EyeOff size={16} /> : <Eye size={16} />}
            </IconButton>
          </div>
          {preview && (
            <p className="text-[12.5px] text-fg-2" aria-live="polite">
              {kind === 'unknown' ? <span className="text-bad">Not a key Ghostlink knows.</span> : kind === 'cli' ? <span className="text-bad">An OpenVPN profile: import it with the command line.</span> : <span>{previewText(preview)}</span>}
            </p>
          )}
        </div>
        <Note>The key holds private credentials. Ghostlink keeps it in the local service and never puts it in logs or connection details.</Note>
        <Note>
          OpenVPN profiles (.ovpn) are imported with the command line: <span className="mono">ghostlink profile import --name NAME file.ovpn</span>.
        </Note>
        {error && <Note tone="bad">{error}</Note>}
        <input
          ref={file}
          type="file"
          accept=".conf,.txt"
          hidden
          onChange={e => {
            const f = e.target.files?.[0];
            e.target.value = '';
            if (f) importFile(f);
          }}
        />
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
