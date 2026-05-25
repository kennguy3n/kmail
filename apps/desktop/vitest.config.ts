import { defineConfig } from 'vitest/config';
import path from 'node:path';

// Vitest config for the renderer-side unit tests.
//
// We deliberately do NOT load the napi `@kmail/sdk-native`
// addon during tests. The vite.config.ts alias that blocks the
// import in the renderer bundle also applies here — every test
// either uses a stub `KMailBridge` (preferred) or, if it needs
// to exercise main-process behaviour, runs as an Electron
// integration test (those land in a follow-up PR alongside an
// electron-mocha runner).
//
// The `resolve.alias` block MUST be kept in sync with
// `vite.config.ts`'s alias — vitest doesn't automatically
// inherit from the sibling vite config when neither is the
// `defineProject` / merge form. Keeping the alias here means a
// test file that accidentally `import`s from `@kmail/sdk-native`
// fails loudly at module-load time (with the block module's
// "this is the renderer, you can't do that" error) instead of
// silently pulling a real .node addon into Node's require cache.
export default defineConfig({
  resolve: {
    alias: {
      '@kmail/sdk-native': path.resolve(
        __dirname,
        'src/kmail/sdk-native.block.ts',
      ),
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
});
