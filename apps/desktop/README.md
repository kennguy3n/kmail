# KMail Desktop (Electron)

Electron + React desktop client that consumes the KMail SDK via
the napi-rs binding at `sdk/kmail-napi/`. The renderer is a thin
React presentation layer; every byte of protocol state, every
offline-cached row, and every crypto operation lives in the Rust
SDK loaded by the Electron main process.

## Why Electron over a native shell

Three reasons:

1. **Component reuse with KChat.** KMail ships inside KChat as a
   first-class pane. KChat's desktop client is already an
   Electron app, and bundling KMail as an Electron app lets us
   reuse the same React component library (forms, modals,
   notifications, design tokens) instead of maintaining a
   parallel Cocoa / WinUI stack.
2. **Cross-platform parity.** macOS / Windows / Linux all see
   identical UX with one codebase; a native shell would require
   three separate teams.
3. **Security model is acceptable for our threat model.** The
   `contextIsolation: true` + `nodeIntegration: false` +
   `sandbox: true` defaults Electron 31 ships with close the
   classic "remote-script-runs-`require`" attack vector. The
   napi-rs addon loads only in the main process; the renderer
   talks to it over a strictly typed IPC bridge. See
   `electron/main.ts` for the full hardening checklist.

## Layout

```
apps/desktop/
├── electron/
│   ├── main.ts        # main process: SDK lifecycle + IPC handlers
│   └── preload.ts     # contextBridge → window.kmail (typed)
├── src/
│   ├── App.tsx        # top-level shell
│   ├── main.tsx       # React entry + HashRouter
│   ├── kmail/
│   │   ├── client.ts          # renderer-side typed wrapper
│   │   ├── client.test.ts     # vitest: parity + error parsing
│   │   ├── errors.ts          # KMailError + tag→kind mapping
│   │   ├── preload.d.ts       # ambient types for window.kmail
│   │   └── sdk-native.block.ts # blocks accidental renderer-side imports
│   └── pages/
│       ├── Inbox.tsx
│       └── Compose.tsx
├── index.html
├── vite.config.ts
├── electron-builder.yml
└── tsconfig*.json
```

## Local dev

Prereqs:
- Node 20 LTS (the napi-rs binary is built against Node's ABI
  via N-API, so any 20.x point release is fine).
- A built `@kmail/sdk-native` package (run `npm run build` in
  `sdk/kmail-napi/` first — this emits the .node file the
  Electron main process loads at startup).

Steps:
```bash
cd sdk/kmail-napi && npm install && npm run build
cd ../../apps/desktop && npm install
npm run start              # builds renderer + main, launches Electron
```

For hot-reload dev (Vite watches the renderer; restart Electron
manually on main-process changes):
```bash
npm run dev                # in one terminal
VITE_DEV_SERVER_URL=http://localhost:5173 npx electron .
```

Session params (BFF URL / bearer token / SQLite path) come from
either:
- `VITE_KMAIL_*` env vars in dev, or
- a hash-fragment on the renderer URL (`#bff=...&token=...&db=...`)
  in production. The parent KChat shell launches the desktop
  client with these baked in.

## Tests

Renderer-side unit tests run under vitest with jsdom:
```bash
npm run test
```

They cover:
- Wire-format JSON parity with the Swift / Kotlin facades
  (`encodeWireFormatDraft` mirrors `makeKMailWireFormatJSONEncoder`
  on iOS and `wireFormatJson` on Android).
- Error-tag parsing: every prefix the napi binding emits
  (`[STORE]`, `[RATE_LIMIT]`, etc.) maps to a typed `KMailError`
  kind, with `[INTERNAL]` as the fallback.
- IPC bridge dispatch via stub `KMailBridge`.

Electron main-process integration tests (real Electron host,
real SDK addon, real SQLite) land in a follow-up PR alongside
an electron-mocha runner.

## Packaging

```bash
npm run package           # current platform / arch
npm run package:mac
npm run package:win
npm run package:linux
```

Outputs land in `dist/installers/`. Code signing requires
`CSC_LINK` + `CSC_KEY_PASSWORD` (Authenticode for Windows, Apple
Developer cert for macOS). Without those env vars, signing is
skipped — the resulting installer will trigger SmartScreen /
Gatekeeper warnings on the user's machine.

## CI

`.github/workflows/sdk-build-napi.yml` cross-builds the napi
`.node` addon for five targets:

| Runner             | Target triple                  | Output     |
| ------------------ | ------------------------------ | ---------- |
| `macos-14` (arm64) | `aarch64-apple-darwin`         | `.node`    |
| `macos-13` (x64)   | `x86_64-apple-darwin`          | `.node`    |
| `ubuntu-latest`    | `x86_64-unknown-linux-gnu`     | `.node`    |
| `ubuntu-latest`    | `aarch64-unknown-linux-gnu`    | `.node` (cross) |
| `windows-latest`   | `x86_64-pc-windows-msvc`       | `.node`    |

Each artifact is uploaded as a GHA artifact so a follow-up
publish workflow can bundle them into the `@kmail/sdk-native`
npm package's `optionalDependencies`.
