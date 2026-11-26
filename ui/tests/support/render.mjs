// Renders the UI's React components to HTML in Node, without a browser and without a new
// dependency: Vite (already a dev dependency, it builds the UI) loads the .tsx sources through
// `ssrLoadModule`, with the same aliases as the real builds, and react-dom/server turns an element
// into a string. Effects do not run in a server render, so nothing polls and nothing is fetched:
// what is rendered is the first paint of a component, which is where "a placeholder, not an empty
// message" and "room for the axis labels" are decided.
//
// This file is not a test (the test script runs tests/*.test.mjs only).
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';
import { renderToString } from 'react-dom/server';

export async function startRenderer() {
  const root = fileURLToPath(new URL('../../', import.meta.url));
  const vite = await createServer({
    root,
    configFile: false,
    logLevel: 'error',
    appType: 'custom',
    // No file watcher, no hot reload and no dependency scan: a server that only loads modules.
    server: { middlewareMode: true, hmr: false, watch: null },
    optimizeDeps: { noDiscovery: true },
    resolve: { alias: { '@shared': fileURLToPath(new URL('../../shared', import.meta.url)), '@admin': fileURLToPath(new URL('../../admin', import.meta.url)) } },
  });
  return {
    // A module of the UI, by its path from ui/ ("/shared/Bars.tsx").
    load: path => vite.ssrLoadModule(path),
    render: element => renderToString(element),
    close: () => vite.close(),
  };
}
