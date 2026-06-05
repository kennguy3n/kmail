// Electron main process for the KMail desktop client.
//
// Responsibilities:
//
//   1. Open the napi-rs `@kmail/sdk-native` addon at startup. The
//      SDK owns the SQLite cache, JMAP transport, and crypto —
//      the renderer is a thin presentation layer.
//   2. Create the BrowserWindow with the hardened web preferences
//      Chromium requires for a desktop email client (context
//      isolation on, node integration off, sandbox on, web
//      security on, remote module disabled).
//   3. Bridge SDK methods to the renderer over typed IPC. Every
//      handler validates inputs and surfaces typed errors so the
//      renderer can show actionable messages without parsing
//      stringly-typed exception payloads.
//   4. Wire system-level integrations: tray icon with quick
//      actions, native notifications on push, single-instance
//      lock, dock-bounce on macOS.
//   5. Tear the SDK down cleanly on quit so the SQLite WAL gets
//      checkpointed and the JMAP push session unsubscribes.

import {
  app,
  BrowserWindow,
  ipcMain,
  Menu,
  Notification,
  shell,
  Tray,
  nativeImage,
} from 'electron';
import path from 'node:path';

// `@kmail/sdk-native` is the napi-rs addon. Loaded synchronously
// at startup because the SDK's open() call runs SQLite schema
// migrations and we want any "DB locked" / "schema mismatch"
// error surfaced before we paint the window — not in the middle
// of the user's first interaction.
//
// The require is wrapped in a function so test harnesses can
// stub it. In production it resolves to the .node file emitted
// by `napi build` (path differs per target triple, resolved by
// the @napi-rs runtime).
import type {
  JsClientConfig,
  JsEmailSummary,
  JsMailbox,
  JsPushIngestOutcome,
  JsSyncSummary,
  KMailClientJs,
} from '@kmail/sdk-native';
// eslint-disable-next-line @typescript-eslint/no-require-imports
const nativeSdk = require('@kmail/sdk-native') as {
  KMailClientJs: {
    open(config: JsClientConfig): KMailClientJs;
  };
  // Mirror of `default_client_config(...)` exposed by the napi
  // crate. The Rust side derives every default from a fresh
  // `ClientConfig::new(...)`, so this is the single source of
  // truth for "what does the SDK default to?" on the desktop —
  // see `sdk/kmail-napi/src/lib.rs::default_client_config` and
  // its `default_client_config_mirrors_core_defaults` test.
  defaultClientConfig(
    bffUrl: string,
    bearerToken: string,
    databasePath: string,
  ): JsClientConfig;
};

// Electron main is always compiled to CommonJS (see
// `tsconfig.electron.json`'s `module: CommonJS`), so __dirname
// is always available at runtime. We capture it under a local
// alias to make grep'ing for path resolutions easy.
const dirname = __dirname;

// ---------------------------------------------------------------
// State
// ---------------------------------------------------------------

interface KMailSession {
  client: KMailClientJs;
  // Mutable so token rotation (set-bearer-token / open with new
  // token) can stay accurate without tearing down the session.
  config: JsClientConfig;
}

let session: KMailSession | null = null;
let mainWindow: BrowserWindow | null = null;
let tray: Tray | null = null;

// ---------------------------------------------------------------
// Single-instance lock
// ---------------------------------------------------------------

// Email clients are *especially* bad to run multiple copies of —
// two processes hammering the same SQLite cache + outbox would
// race themselves into corruption. Electron's
// `requestSingleInstanceLock` returns false on the second
// instance; we quit it and focus the first one's window.
const gotTheLock = app.requestSingleInstanceLock();
if (!gotTheLock) {
  app.quit();
}

app.on('second-instance', () => {
  if (mainWindow) {
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.focus();
  }
});

// ---------------------------------------------------------------
// Window
// ---------------------------------------------------------------

function createWindow(): BrowserWindow {
  const win = new BrowserWindow({
    width: 1280,
    height: 800,
    minWidth: 960,
    minHeight: 600,
    title: 'KMail',
    backgroundColor: '#0f172a',
    webPreferences: {
      // Load the typed IPC bridge into the renderer. Compiled
      // to dist/electron/preload.js by tsc; a sibling
      // package.json with `"type": "commonjs"` (emitted by the
      // build:main script) ensures Node treats it as CommonJS
      // even though the parent package.json declares ESM.
      preload: path.join(dirname, 'preload.js'),
      // The full Electron security checklist. Each of these
      // closes off an attack surface that desktop email clients
      // historically suffered from:
      //   - contextIsolation: render-side `window` cannot reach
      //     `require` / `process` / Node primitives.
      //   - nodeIntegration: off so even `<script>` injection
      //     in an email preview can't `require('child_process')`.
      //   - sandbox: Chromium kernel-level sandbox on the
      //     renderer process.
      //   - webSecurity: same-origin policy enforced.
      //   - allowRunningInsecureContent / experimentalFeatures /
      //     navigateOnDragDrop: all default-off; spelled out
      //     here so a future change cannot silently flip them.
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      webSecurity: true,
      allowRunningInsecureContent: false,
      experimentalFeatures: false,
      navigateOnDragDrop: false,
      spellcheck: true,
    },
    show: false,
  });

  // Don't show until the renderer fires `ready-to-show` — avoids
  // a flash of white background on cold start.
  win.once('ready-to-show', () => win.show());

  // Open external links in the user's real browser, NOT a new
  // BrowserWindow. Stops "evil email link opens phishing page
  // inside our Electron shell" attacks. The protocol allowlist
  // hardens against a compromised renderer that tries to pass
  // `file://`, `smb://`, `vbscript:`, etc. through shell.openExternal
  // — that would invoke the OS's default handler for those
  // schemes and could trigger native code execution outside the
  // sandbox.
  win.webContents.setWindowOpenHandler(({ url }) => {
    if (isSafeExternalUrl(url)) {
      void shell.openExternal(url);
    } else {
      console.warn('[security] blocked window.open for unsafe URL:', url);
    }
    return { action: 'deny' };
  });

  // Block in-place navigation to anything except the bundled
  // index.html. The renderer should never load arbitrary URLs
  // — those are external by definition.
  win.webContents.on('will-navigate', (event, url) => {
    const allowed = win.webContents.getURL();
    if (url !== allowed) {
      event.preventDefault();
      if (isSafeExternalUrl(url)) {
        void shell.openExternal(url);
      } else {
        console.warn('[security] blocked navigation to unsafe URL:', url);
      }
    }
  });

  // Dev: connect to the Vite dev server. Prod: load the bundled
  // index.html from disk.
  if (process.env.VITE_DEV_SERVER_URL) {
    void win.loadURL(process.env.VITE_DEV_SERVER_URL);
    win.webContents.openDevTools({ mode: 'detach' });
  } else {
    void win.loadFile(path.join(dirname, '../renderer/index.html'));
  }

  return win;
}

// ---------------------------------------------------------------
// Tray
// ---------------------------------------------------------------

function createTray(): Tray {
  // The tray icon ships as an empty PNG placeholder for now —
  // the design team supplies the real glyph in a follow-up. The
  // dimensions (16x16 light, 32x32 dark) are correct so macOS
  // template-image rendering won't blur.
  const iconPath = path.join(dirname, '../../public/tray-icon.png');
  const icon = nativeImage
    .createFromPath(iconPath)
    .resize({ width: 16, height: 16 });
  if (process.platform === 'darwin') {
    icon.setTemplateImage(true);
  }

  const t = new Tray(icon.isEmpty() ? nativeImage.createEmpty() : icon);
  t.setToolTip('KMail');

  const contextMenu = Menu.buildFromTemplate([
    {
      label: 'Open KMail',
      click: () => {
        if (mainWindow) {
          mainWindow.show();
          mainWindow.focus();
        }
      },
    },
    {
      label: 'Sync now',
      click: () => {
        if (!session) return;
        // Fire-and-forget: errors surface in the renderer via
        // the next sync trigger. The tray click is best-effort.
        session.client.sync().catch((err) => {
          console.error('tray sync failed', err);
        });
      },
    },
    { type: 'separator' },
    { label: 'Quit', role: 'quit' },
  ]);
  t.setContextMenu(contextMenu);

  t.on('click', () => {
    if (mainWindow) {
      mainWindow.isVisible() ? mainWindow.hide() : mainWindow.show();
    }
  });

  return t;
}

// ---------------------------------------------------------------
// IPC handlers
// ---------------------------------------------------------------

// Error tag prefixes match those emitted by `napi_err()` in
// `sdk/kmail-napi/src/lib.rs` plus `[INTERNAL]` for errors
// originating in this main process (e.g. requireSession()).
// The renderer parses these via `src/kmail/errors.ts` to
// build typed exceptions. Keeping `[INTERNAL]` in the list
// prevents `sanitiseError()` from double-wrapping a message
// that already starts with `[INTERNAL]`.
const KMAIL_ERROR_PREFIXES = [
  '[STORE]',
  '[TRANSPORT]',
  '[AUTH]',
  '[FORBIDDEN]',
  '[NOT_FOUND]',
  '[RATE_LIMIT]',
  '[JMAP]',
  '[PROTOCOL]',
  '[HTTP_CLIENT]',
  '[SYNC_DIVERGED]',
  '[DECRYPTION]',
  '[KDF]',
  '[KEYSTORE]',
  '[ARG]',
  '[CANCELLED]',
  '[INTERNAL]',
] as const;

// Protocol allowlist for `shell.openExternal`. Only schemes that
// open a browser or a mail composer are permitted; everything
// else (file://, smb://, vbscript:, javascript:, custom
// app-specific schemes, etc.) is blocked. The renderer is
// already sandboxed but this is a second line of defence if a
// future XSS in email preview rendering tries to escape via
// `window.open()`.
const SAFE_EXTERNAL_PROTOCOLS: ReadonlySet<string> = new Set([
  'http:',
  'https:',
  'mailto:',
]);

function isSafeExternalUrl(rawUrl: string): boolean {
  try {
    const parsed = new URL(rawUrl);
    return SAFE_EXTERNAL_PROTOCOLS.has(parsed.protocol);
  } catch {
    // Invalid URL strings can never be safely opened.
    return false;
  }
}

// Lock down which prefix the renderer is allowed to see. If the
// SDK emits a new error variant, the prefix must be added here
// AND to `src/kmail/errors.ts` to maintain typed-exception parity.
function sanitiseError(err: unknown): string {
  if (err instanceof Error) {
    const hasKnownPrefix = KMAIL_ERROR_PREFIXES.some((p) =>
      err.message.startsWith(p),
    );
    return hasKnownPrefix ? err.message : `[INTERNAL] ${err.message}`;
  }
  return `[INTERNAL] ${String(err)}`;
}

// napi-rs maps Rust `Option<String>::None` to JavaScript `null`,
// but TypeScript callers that simply omit `accountId` produce
// `undefined`. The two are semantically equivalent in this codebase
// — both mean "no account_id" — but `===` would treat them as
// distinct. Collapse to a single `null` sentinel before comparison
// so the identity check in `kmail:open` is robust to either source.
function normaliseAccountId(id: string | null | undefined): string | null {
  return id ?? null;
}

function requireSession(): KMailSession {
  if (!session) {
    // This is an [INTERNAL] error rather than a SDK [ARG] because
    // it indicates the renderer called an SDK method before
    // `kmail:open` resolved — a programming error in the renderer,
    // not a misconfiguration of the SDK.
    throw new Error(
      '[INTERNAL] SDK not initialised — call kmail:open first',
    );
  }
  return session;
}

function registerIpc(): void {
  ipcMain.handle(
    'kmail:open',
    async (_evt, config: JsClientConfig): Promise<void> => {
      try {
        if (session) {
          // Idempotent: opening twice with matching bffUrl /
          // databasePath / accountId is a no-op. The bearer
          // token is *not* part of the identity check because
          // tokens rotate (OIDC refresh, KChat re-auth) far more
          // often than the other fields — if it differs we
          // forward to setBearerToken so the existing SQLite +
          // JMAP transport carries on with fresh credentials.
          // Everything else differing means a different account
          // / shard, which would clobber the SQLite WAL: reject
          // and require an explicit kmail:close first.
          //
          // `normaliseAccountId` collapses `undefined` (TS-side
          // omitted field) and `null` (napi mapping of Rust
          // `Option<String>::None` — what `defaultClientConfig`
          // returns) to a single `null` sentinel so the identity
          // check doesn't falsely reject a caller that seeded the
          // first open with `readSessionParams()` (which produces
          // `undefined` for missing accountId) and the second open
          // with `defaultClientConfig(...)` (which returns `null`).
          // Both call sites are within the same renderer process
          // and represent the same "no account" intent.
          const sameIdentity =
            session.config.bffUrl === config.bffUrl &&
            session.config.databasePath === config.databasePath &&
            normaliseAccountId(session.config.accountId) ===
              normaliseAccountId(config.accountId);
          if (sameIdentity) {
            if (session.config.bearerToken !== config.bearerToken) {
              session.client.setBearerToken(config.bearerToken);
              session.config = { ...session.config, bearerToken: config.bearerToken };
            }
            return;
          }
          throw new Error(
            '[ARG] SDK already open with a different config; call kmail:close first',
          );
        }
        const client = nativeSdk.KMailClientJs.open(config);
        session = { client, config };
      } catch (err) {
        throw new Error(sanitiseError(err));
      }
    },
  );

  ipcMain.handle('kmail:close', async (): Promise<void> => {
    // No explicit close method on the napi surface — drop the
    // reference and let napi's Drop handle SQLite WAL checkpoint
    // and JMAP push unsubscribe. Setting `session = null` makes
    // the JS object eligible for GC; the Rust-side Drop runs on
    // the next napi finalize tick (typically the same event loop
    // turn for `Arc<KMailClient>` since napi-rs uses a finalizer
    // queue, not generational GC).
    session = null;
  });

  // Every SDK-backed handler funnels its body through this helper
  // so the [INTERNAL] "SDK not initialised" error from
  // requireSession() and the [TAGGED] errors from the SDK both
  // travel through the same sanitiseError() gate before being
  // serialised across the IPC boundary. Without this the
  // "called before open" error would skip sanitisation — it
  // already starts with [INTERNAL], so the renderer parses it
  // correctly, but uniformity guarantees that any future
  // requireSession() rephrasing can't accidentally leak an
  // un-prefixed message into the renderer.
  async function inSession<T>(
    op: (s: KMailSession) => T | Promise<T>,
  ): Promise<T> {
    try {
      const s = requireSession();
      return await op(s);
    } catch (err) {
      throw new Error(sanitiseError(err));
    }
  }

  ipcMain.handle('kmail:sync', async (): Promise<JsSyncSummary> => {
    return inSession((s) => s.client.sync());
  });

  ipcMain.handle(
    'kmail:set-bearer-token',
    async (_evt, token: string): Promise<void> => {
      return inSession((s) => {
        s.client.setBearerToken(token);
        s.config = { ...s.config, bearerToken: token };
      });
    },
  );

  ipcMain.handle('kmail:invalidate-session', async (): Promise<void> => {
    return inSession((s) => s.client.invalidateSession());
  });

  ipcMain.handle(
    'kmail:cached-mailboxes',
    async (): Promise<JsMailbox[]> => {
      return inSession((s) => s.client.cachedMailboxes());
    },
  );

  ipcMain.handle(
    'kmail:cached-emails',
    async (
      _evt,
      mailboxId: string,
      limit: number,
    ): Promise<JsEmailSummary[]> => {
      return inSession((s) => s.client.cachedEmailsInMailbox(mailboxId, limit));
    },
  );

  ipcMain.handle(
    'kmail:send-email',
    async (_evt, draftJson: string): Promise<string> => {
      return inSession((s) => s.client.sendEmail(draftJson));
    },
  );

  ipcMain.handle(
    'kmail:enqueue-set-keywords',
    async (
      _evt,
      emailId: string,
      keywordsJson: string,
    ): Promise<void> => {
      return inSession((s) => {
        s.client.enqueueSetKeywords(emailId, keywordsJson);
      });
    },
  );

  // Read the SDK's canonical default config — used by the renderer
  // to seed a config object instead of hard-coding literals (which
  // would replicate the cross-binding drift bug the napi helper was
  // designed to eliminate). Sync — the underlying napi function is
  // a pure constructor of a `JsClientConfig` from a fresh
  // `ClientConfig::new(...)`, no I/O.
  ipcMain.handle(
    'kmail:default-client-config',
    async (
      _evt,
      bffUrl: string,
      bearerToken: string,
      databasePath: string,
    ): Promise<JsClientConfig> => {
      try {
        return nativeSdk.defaultClientConfig(bffUrl, bearerToken, databasePath);
      } catch (err) {
        throw new Error(sanitiseError(err));
      }
    },
  );

  // Native notification (used when a push event lands and the
  // window is backgrounded). Renderer can also fire its own
  // HTML5 Notification, but those don't survive minimise on
  // some Linux WMs — the system-level Notification does.
  ipcMain.handle(
    'kmail:notify',
    async (
      _evt,
      title: string,
      body: string,
    ): Promise<void> => {
      showOsNotification(title, body);
    },
  );

  // Ingest a transport-level push payload (the `data` map from
  // whatever push channel the desktop wires up) entirely in the
  // main process: the SDK parses it, caches a preview row, and
  // returns a ready-to-render notification. We surface the OS
  // notification here — closest to the `Notification` API and so a
  // backgrounded window still alerts — and hand the parsed outcome
  // back to the renderer so it can refresh the inbox and decide
  // whether to kick off a `sync()` (a push is a hint, not an
  // authoritative delta cursor, so `needsDeltaSync` is almost
  // always true).
  ipcMain.handle(
    'kmail:ingest-push',
    async (
      _evt,
      data: Record<string, string>,
    ): Promise<JsPushIngestOutcome> => {
      return inSession((s) => {
        const outcome = s.client.ingestPushDelivery(data);
        if (outcome.notification) {
          showOsNotification(
            outcome.notification.title,
            outcome.notification.body,
          );
        }
        return outcome;
      });
    },
  );
}

// Show a system-level notification that focuses the main window on
// click. Shared by the `kmail:notify` (renderer-driven) and
// `kmail:ingest-push` (SDK-driven) handlers so both honour the
// `Notification.isSupported()` guard and the click-to-focus
// behaviour identically.
function showOsNotification(title: string, body: string): void {
  if (!Notification.isSupported()) return;
  const n = new Notification({ title, body, silent: false });
  n.on('click', () => {
    if (mainWindow) {
      mainWindow.show();
      mainWindow.focus();
    }
  });
  n.show();
}

// ---------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------

void app.whenReady().then(() => {
  registerIpc();
  mainWindow = createWindow();
  tray = createTray();

  app.on('activate', () => {
    // macOS: clicking the dock icon when no windows are open
    // recreates one. Matches Mail.app behaviour.
    if (BrowserWindow.getAllWindows().length === 0) {
      mainWindow = createWindow();
    }
  });
});

app.on('window-all-closed', () => {
  // macOS keeps apps alive until Cmd-Q; every other platform
  // quits when the last window closes.
  if (process.platform !== 'darwin') {
    app.quit();
  }
});

app.on('before-quit', () => {
  // Drop the SDK reference so the napi Drop runs and SQLite
  // gets a clean WAL checkpoint. The tray must also be released
  // because Electron's GC won't tear it down before the event
  // loop exits, which leaves a ghost tray icon on Linux.
  session = null;
  if (tray) {
    tray.destroy();
    tray = null;
  }
});
