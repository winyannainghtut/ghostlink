// Copying text to the clipboard without ever throwing. The clipboard API can refuse (the window
// has lost focus, a permission is denied, the page is not a secure context), so an older way is
// tried next, and the caller learns whether anything was copied. The two ways are passed in, so
// the tests run this without a browser (node --experimental-strip-types).
export type ClipboardEnv = {
  // navigator.clipboard.writeText
  write?: (text: string) => Promise<void>;
  // document.execCommand('copy') on a hidden field: true when it copied
  legacy?: (text: string) => boolean;
};

export async function copyText(text: string, env: ClipboardEnv): Promise<boolean> {
  if (env.write) {
    try {
      await env.write(text);
      return true;
    } catch {
      /* refused: try the older way */
    }
  }
  if (env.legacy) {
    try {
      return env.legacy(text);
    } catch {
      return false;
    }
  }
  return false;
}

// The two ways in a browser. The field of the older way goes inside the open sheet, if there is
// one: the rest of the page is inert while a modal sheet is open, and selection fails there.
export const browserClipboard: ClipboardEnv = {
  write: text => navigator.clipboard.writeText(text),
  legacy: text => {
    const host = [...document.querySelectorAll('dialog[open]')].pop() ?? document.body;
    const field = document.createElement('textarea');
    field.value = text;
    field.setAttribute('readonly', '');
    field.setAttribute('aria-hidden', 'true');
    field.style.position = 'fixed';
    field.style.top = '0';
    field.style.opacity = '0';
    const before = document.activeElement as HTMLElement | null;
    host.append(field);
    field.focus();
    field.select();
    try {
      return document.execCommand('copy');
    } finally {
      field.remove();
      before?.focus?.();
    }
  },
};
