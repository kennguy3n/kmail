// Ambient typings for the `window.kmail` bridge exposed by
// `electron/preload.ts`.
//
// The napi-rs `index.d.ts` at `sdk/kmail-napi/index.d.ts` is the
// single source of truth for the JS-side shape of every record
// the addon exports. We re-export the relevant interfaces here
// via `import type`, which:
//
//   * Is stripped by esbuild BEFORE Vite's resolver runs, so the
//     `@kmail/sdk-native -> sdk-native.block.ts` alias in
//     `vite.config.ts` / `vitest.config.ts` never fires for these
//     references. The block continues to catch any *runtime*
//     import attempt from the renderer.
//   * Is honoured by `tsc -p tsconfig.json` via Node-style module
//     resolution (the renderer tsconfig has no `paths` entry, so
//     TypeScript reads `package.json` -> `types: "index.d.ts"`
//     directly from the linked `@kmail/sdk-native` package). A
//     field rename in the Rust `#[napi(object)]` struct now
//     ripples to the renderer typecheck instead of silently
//     diverging from the manual re-declaration that used to live
//     here.
//
// `KMailBridge` is declared locally (not in the napi package)
// because it describes the *IPC* surface the preload script
// exposes, not the SDK surface itself. Every method here MUST
// match an `ipcMain.handle(...)` registration in `electron/
// main.ts` AND a `bridge.<method>` entry in `electron/
// preload.ts`. The interface is what binds the three layers
// together at compile time.

import type {
  JsClientConfig,
  JsEmailAddress,
  JsEmailSummary,
  JsMailbox,
  JsSyncSummary,
} from '@kmail/sdk-native';

export type {
  JsClientConfig,
  JsEmailAddress,
  JsEmailSummary,
  JsMailbox,
  JsSyncSummary,
};

export interface KMailBridge {
  open(config: JsClientConfig): Promise<void>;
  close(): Promise<void>;
  sync(): Promise<JsSyncSummary>;
  setBearerToken(token: string): Promise<void>;
  invalidateSession(): Promise<void>;
  cachedMailboxes(): Promise<JsMailbox[]>;
  cachedEmails(mailboxId: string, limit: number): Promise<JsEmailSummary[]>;
  sendEmail(draftJson: string): Promise<string>;
  enqueueSetKeywords(emailId: string, keywordsJson: string): Promise<void>;
  notify(title: string, body: string): Promise<void>;
}

declare global {
  interface Window {
    kmail: KMailBridge;
  }
}

export {};
