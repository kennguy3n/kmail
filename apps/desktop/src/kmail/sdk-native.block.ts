// Vite alias target for `@kmail/sdk-native` in the renderer.
//
// The renderer is NOT allowed to load the napi-rs `.node` addon
// directly. All SDK interaction must go through the IPC bridge
// in `electron/preload.ts` (exposed to the renderer as
// `window.kmail`).
//
// Why this is enforced at the Vite-alias level rather than
// "developer discipline":
//
//   - Electron 31 ships with `contextIsolation: true` and
//     `nodeIntegration: false` defaults — but those guard the
//     *runtime* surface. A `require('@kmail/sdk-native')` in a
//     renderer file would crash at load (ReferenceError on
//     `require`), but only at runtime. By blocking the import at
//     bundle time we move the failure left to the build, where
//     a CI typecheck/build job will catch it before merge.
//   - The napi-rs addon links against Node.js's `napi_*` symbols.
//     The renderer runs as a sandboxed Chromium process that
//     doesn't have those symbols available — so even if someone
//     bypassed the build-time block, the runtime `dlopen` would
//     fail with a confusing `unresolved external symbol` error.
//     Failing the build is friendlier than a cryptic runtime crash.
//
// Importing this module throws synchronously at module-eval time
// so a renderer that accidentally pulls in the SDK addon gets a
// clear, immediate error pointing at the IPC bridge instead of a
// silent missing-symbol failure.

throw new Error(
  '@kmail/sdk-native must not be imported from the Electron renderer. ' +
    "Use window.kmail (defined in electron/preload.ts) instead.",
);

export {};
