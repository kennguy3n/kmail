import { defineConfig } from 'vitest/config';

// Vitest config for the renderer-side unit tests.
//
// We deliberately do NOT load the napi `@kmail/sdk-native`
// addon during tests. The vite.config.ts alias that blocks the
// import in the renderer bundle also applies here — every test
// either uses a stub `KMailBridge` (preferred) or, if it needs
// to exercise main-process behaviour, runs as an Electron
// integration test (those land in a follow-up PR alongside an
// electron-mocha runner).
export default defineConfig({
  test: {
    globals: true,
    environment: 'jsdom',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
});
