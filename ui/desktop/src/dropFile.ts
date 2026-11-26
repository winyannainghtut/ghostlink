// A file dropped on Add access key: what to accept, and the state of the drop zone. The text of the
// file is then handled as if it had been pasted (keyPreview.ts decides what it is), so a dropped
// WireGuard file, an access key saved in a text file and an OpenVPN profile end up where pasting
// them would. No imports, so the tests run it directly (node --experimental-strip-types).

// The size of the largest file read: the same cap as the file picker and as the service's own
// limit for one credential (64 KiB).
export const MAX_FILE_BYTES = 65536;

export const OPENVPN_NOTE = 'This is an OpenVPN profile. The app adds access keys and WireGuard or AmneziaWG files; import an OpenVPN profile with the command line.';

// Whether a drag carries files (a dragged text selection or link does not, and is left to the page).
// `types` is the drag's `dataTransfer.types`.
export function hasFiles(types: ArrayLike<string> | null | undefined): boolean {
  return !!types && Array.prototype.includes.call(types, 'Files');
}

// dragenter and dragleave also fire for every element the pointer crosses inside the zone, so the
// zone counts them: it is entered at 1 and left at 0. A drop, or anything unbalanced, ends at 0.
export function nextDepth(depth: number, event: 'enter' | 'leave' | 'drop'): number {
  if (event === 'enter') return depth + 1;
  if (event === 'leave') return Math.max(0, depth - 1);
  return 0;
}

export type DropFile = { name: string; size: number };

// The file to read out of what was dropped, or what to tell the person. One file only: two would
// leave open which one is meant.
export function pickDropped<T extends DropFile>(files: readonly T[]): { file: T } | { error: string } {
  if (files.length === 0) return { error: 'Nothing was dropped. Drop a WireGuard or AmneziaWG .conf file, or a text file with an access key.' };
  if (files.length > 1) return { error: 'Drop one file at a time.' };
  const file = files[0];
  // An OpenVPN profile is a text file too, and may be larger than the cap: say what to do with it
  // rather than that it is too large.
  if (/\.ovpn$/i.test(file.name)) return { error: OPENVPN_NOTE };
  if (file.size > MAX_FILE_BYTES) return { error: 'That file is too large to be a WireGuard configuration or an access key.' };
  return { file };
}

// The file's text, or why it cannot be used. A file that is not text (a picture, an archive) comes
// out of a text read with NUL bytes or with U+FFFD, the replacement for bytes that are not UTF-8;
// neither is in a configuration file or a key.
export function droppedText(text: string): { text: string } | { error: string } {
  if (/[\u0000\uFFFD]/.test(text)) return { error: 'That file is not a text file. Drop a WireGuard or AmneziaWG .conf file, or a text file with an access key.' };
  if (!text.trim()) return { error: 'That file is empty.' };
  if (text.length > MAX_FILE_BYTES) return { error: 'That file is too large to be a WireGuard configuration or an access key.' };
  return { text };
}

// A file dropped anywhere else in the window must not make the page open it (WebView2 would
// navigate to the file and leave the app). The sheet that takes files has handled the event, and
// said so with preventDefault, before this runs; for any other drop the cursor says "not allowed".
// `target` is the window.
type DragLike = { dataTransfer: { types: ArrayLike<string>; dropEffect: string } | null; defaultPrevented: boolean; preventDefault(): void };
export function guardFileDrops(target: { addEventListener(type: string, listener: (event: any) => void): void }): void {
  for (const type of ['dragover', 'drop']) {
    target.addEventListener(type, (event: DragLike) => {
      if (!hasFiles(event.dataTransfer?.types)) return;
      const taken = event.defaultPrevented;
      event.preventDefault();
      if (type === 'dragover' && !taken && event.dataTransfer) event.dataTransfer.dropEffect = 'none';
    });
  }
}
