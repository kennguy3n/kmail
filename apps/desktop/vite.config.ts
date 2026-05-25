import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

// Vite config for the Electron renderer.
//
// The renderer is a thin React SPA; the heavy lifting (JMAP
// transport, SQLite cache, crypto) lives in the napi-rs `@kmail/
// sdk-native` addon that the Electron *main* process loads.
//
// The renderer never touches the SDK directly — it talks to main
// over `window.kmail` (the contextBridge surface defined in
// `electron/preload.ts`). That's a hard requirement of the
// Electron security model: the napi `.node` addon must NOT be
// loadable from a sandboxed renderer process, because doing so
// would re-introduce the same Node-in-renderer vulnerability
// surface (`vm.runInThisContext` + filesystem access) that
// `contextIsolation: true` exists to close.
//
// `base: './'` makes the built `index.html` reference its assets
// via relative paths, so it works under Electron's `file://`
// scheme without a custom protocol handler.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist/renderer',
    emptyOutDir: true,
    sourcemap: true,
    target: 'es2022',
    rollupOptions: {
      input: path.resolve(__dirname, 'index.html'),
    },
  },
  server: {
    port: 5173,
    strictPort: true,
  },
  // The renderer never imports `@kmail/sdk-native` — block any
  // accidental import at build time so a future contributor can't
  // sneak a direct Node addon load into the renderer bundle.
  resolve: {
    alias: {
      '@kmail/sdk-native': path.resolve(
        __dirname,
        'src/kmail/sdk-native.block.ts',
      ),
    },
  },
});
