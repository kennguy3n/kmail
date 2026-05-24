// Electron preload script.
//
// Runs in an isolated world between the Chromium renderer and
// the Electron main process. The renderer can't `require()`
// anything (contextIsolation + nodeIntegration off), so the
// preload script is the *only* way to expose Electron / Node
// primitives to the page.
//
// We expose a single object — `window.kmail` — with strictly
// typed methods that wrap the IPC channels registered in
// `main.ts`. Each method returns a Promise so the renderer
// awaits results naturally, and errors flow through normal
// async-throw semantics rather than serialised IPC payloads.
//
// IMPORTANT: every method here MUST match exactly one IPC
// handler registered via `ipcMain.handle(...)`. There is a
// runtime cross-check in `electron/main.test.ts` (when present)
// that enumerates both sides.

import { contextBridge, ipcRenderer } from 'electron';

// The TypeScript types for the napi surface are imported
// purely as types — they don't trigger a runtime load of the
// native addon. The renderer side reuses these types via
// `src/kmail/preload.d.ts`.
import type {
  JsClientConfig,
  JsEmailSummary,
  JsMailbox,
  JsSyncSummary,
} from '@kmail/sdk-native';

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

const bridge: KMailBridge = {
  open: (config) => ipcRenderer.invoke('kmail:open', config),
  close: () => ipcRenderer.invoke('kmail:close'),
  sync: () => ipcRenderer.invoke('kmail:sync'),
  setBearerToken: (token) =>
    ipcRenderer.invoke('kmail:set-bearer-token', token),
  invalidateSession: () => ipcRenderer.invoke('kmail:invalidate-session'),
  cachedMailboxes: () => ipcRenderer.invoke('kmail:cached-mailboxes'),
  cachedEmails: (mailboxId, limit) =>
    ipcRenderer.invoke('kmail:cached-emails', mailboxId, limit),
  sendEmail: (draftJson) => ipcRenderer.invoke('kmail:send-email', draftJson),
  enqueueSetKeywords: (emailId, keywordsJson) =>
    ipcRenderer.invoke('kmail:enqueue-set-keywords', emailId, keywordsJson),
  notify: (title, body) => ipcRenderer.invoke('kmail:notify', title, body),
};

contextBridge.exposeInMainWorld('kmail', bridge);
