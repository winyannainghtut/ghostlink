// Add an access key to this device, or replace the key of a server already here. The key can be an
// ss://, ssconf://, vless:// or vpn:// key, or the text of a WireGuard or AmneziaWG file (pasted, from
// a file, or a file dropped on the sheet): the sheet shows what it is (protocol, host, port) before
// it is added.
import { ClipboardPaste, Eye, EyeOff, FileUp } from 'lucide-react';
import { useEffect, useRef, useState, type DragEvent } from 'react';
import { Button, Field, IconButton, Note, Sheet, useUI } from '@shared/glass';
import { useBusy } from '@shared/hooks';
import { step, type Done } from '@shared/steps';
import { droppedText, hasFiles, nextDepth, OPENVPN_NOTE, pickDropped } from '../dropFile';
import { request, slug, uniqueName, type Profile } from '../ipc';
import { importKind, previewKey, previewText } from '../keyPreview';
import { useService } from '../service';

// A file's text, read in the page (a dropped file's path is not available here, and need not be).
function readText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result ?? ''));
    reader.onerror = () => reject(reader.error ?? new Error('The file could not be read.'));
    reader.readAsText(file);
  });
}

// What the sheet shows while a file is dragged over it: it covers the form, and takes no pointer
// events itself, so the drag keeps hitting the sheet.
function DropZone() {
  return (
    <div
      aria-hidden="true"
      data-drop-zone=""
      className="pointer-events-none absolute inset-0 z-10 grid place-items-center rounded-[22px] border-2 border-dashed border-fg/50 bg-canvas text-center"
    >
      <div className="grid justify-items-center gap-1.5 px-6">
        <FileUp size={28} />
        <span className="text-[15px] font-medium">Drop the file to read it</span>
        <span className="text-[12.5px] text-fg-2">A WireGuard or AmneziaWG .conf file, or a text file with an access key</span>
      </div>
    </div>
  );
}

export function AddKeySheet({ open, onClose, replacing }: { open: boolean; onClose: () => void; replacing: Profile | null }) {
  const { profiles, refresh } = useService();
  const { toast } = useUI();
  const [name, setName] = useState('');
  const [key, setKey] = useState('');
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState('');
  const { busy, run } = useBusy();
  const file = useRef<HTMLInputElement>(null);
  // A file is being dragged over the sheet. dragenter and dragleave fire for every element the
  // pointer crosses, so `depth` counts them (dropFile.ts).
  const [over, setOver] = useState(false);
  const depth = useRef(0);
  // Saving the key and naming it are two requests. When the name fails the key is saved already:
  // the steps that worked are kept, and the next try only names it (a second import would save the
  // same server again under another ID).
  const done = useRef<Done>(new Map());
  useEffect(() => {
    if (!open) {
      setKey('');
      setReveal(false);
      setOver(false);
      depth.current = 0;
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
      setError(OPENVPN_NOTE);
      return;
    }
    if (kind === 'unknown') {
      setError('Paste a key beginning with ss://, ssconf://, vless:// or vpn://, or the text of a WireGuard or AmneziaWG file.');
      return;
    }
    return submit(source, kind === 'file' ? 'file' : 'key', kind === 'file' ? preview?.host || 'WireGuard' : '');
  };

  // The file picker: a WireGuard/AmneziaWG file is read here and sent straight to the service.
  const importFile = async (f: File) => {
    setError('');
    const picked = pickDropped([f]);
    if ('error' in picked) {
      setError(picked.error);
      return;
    }
    let text: string;
    try {
      const read = droppedText(await readText(f));
      if ('error' in read) {
        setError(read.error);
        return;
      }
      text = read.text;
    } catch (e) {
      setError((e as Error).message);
      return;
    }
    if (importKind(previewKey(text)) === 'cli') {
      setError(OPENVPN_NOTE);
      return;
    }
    await submit(text, 'file', f.name.replace(/\.(conf|txt)$/i, ''));
  };

  // A file dropped on the sheet is handled like pasted text: it goes into the box, where the
  // preview says what it is, and the person adds it with the button. (Tauri's own file-drop handler
  // is off for this window, tauri.conf.json `dragDropEnabled`, or it would take the drop first.)
  const accepting = open && !busy && !done.current.has('save');
  const takeDropped = async (files: File[]) => {
    setError('');
    const picked = pickDropped(files);
    if ('error' in picked) {
      setError(picked.error);
      return;
    }
    try {
      const read = droppedText(await readText(picked.file));
      if ('error' in read) {
        setError(read.error);
        return;
      }
      setKey(read.text);
      // A WireGuard file takes its name from the file's, as the file picker does.
      if (!name.trim() && importKind(previewKey(read.text)) === 'file') setName(picked.file.name.replace(/\.(conf|txt)$/i, '').slice(0, 80));
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const drop = {
    onDragEnter: (e: DragEvent) => {
      if (!hasFiles(e.dataTransfer.types)) return;
      e.preventDefault();
      depth.current = nextDepth(depth.current, 'enter');
      setOver(true);
    },
    onDragOver: (e: DragEvent) => {
      if (!hasFiles(e.dataTransfer.types)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = accepting ? 'copy' : 'none';
    },
    onDragLeave: (e: DragEvent) => {
      if (!hasFiles(e.dataTransfer.types)) return;
      depth.current = nextDepth(depth.current, 'leave');
      if (depth.current === 0) setOver(false);
    },
    onDrop: (e: DragEvent) => {
      if (!hasFiles(e.dataTransfer.types)) return;
      e.preventDefault();
      depth.current = nextDepth(depth.current, 'drop');
      setOver(false);
      if (accepting) takeDropped(Array.from(e.dataTransfer.files));
    },
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
      drop={drop}
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
        className="relative grid gap-4"
        onSubmit={e => {
          e.preventDefault();
          add();
        }}
      >
        {over && accepting && <DropZone />}
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
              placeholder={'ss://, ssconf://, vless:// or vpn://\nor the text of a WireGuard or AmneziaWG .conf file\n(or drop the file on this window)'}
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
