import '@shared/styles.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { guardFileDrops } from './dropFile';

// The window's own file-drop handler is off (tauri.conf.json), so the page gets the drops.
guardFileDrops(window);

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
