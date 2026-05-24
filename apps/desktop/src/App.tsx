import { useCallback, useEffect, useState } from 'react';
import { Link, Outlet, useNavigate } from 'react-router-dom';
import { useKMail } from './kmail/client';
import { KMailError } from './kmail/errors';
import { launchHashParams } from './launchParams';
import type { JsMailbox } from './kmail/preload';

// Top-level app shell.
//
// On mount it kicks off a single `sync()` call to refresh the
// local SQLite cache from JMAP. The sidebar reads from the cache
// regardless of whether sync succeeded, so the UI remains usable
// offline.
//
// Authentication, BFF URL, and the database path are read from
// query parameters on the `file://` URL — the parent product
// (KChat) launches the desktop client with these baked in. In
// dev (`npm run dev` against a stub KChat) they fall back to
// the `VITE_*` env vars Vite injects at build time.

interface SessionParams {
  bffUrl: string;
  bearerToken: string;
  databasePath: string;
  accountId?: string;
}

function readSessionParams(): SessionParams | null {
  // 1. Hash-fragment params (production: KChat launches with
  //    `kmail.html#bff=...&token=...&db=...&acct=...`).
  //
  // We read from `launchHashParams` — a snapshot of
  // `window.location.hash` captured at module-load time, BEFORE
  // `<HashRouter>` mounted and claimed ownership of the hash for
  // routing. Reading `window.location.hash` directly here would
  // return the router-normalised value (`/bff=...`), which
  // `URLSearchParams` would parse with `/bff` as the first key.
  // See `src/launchParams.ts` for the full rationale.
  const bffUrl =
    launchHashParams.get('bff') ?? import.meta.env.VITE_KMAIL_BFF_URL;
  const bearerToken =
    launchHashParams.get('token') ?? import.meta.env.VITE_KMAIL_BEARER_TOKEN;
  const databasePath =
    launchHashParams.get('db') ?? import.meta.env.VITE_KMAIL_DATABASE_PATH;
  const accountId =
    launchHashParams.get('acct') ?? import.meta.env.VITE_KMAIL_ACCOUNT_ID;

  if (!bffUrl || !bearerToken || !databasePath) return null;
  return {
    bffUrl,
    bearerToken,
    databasePath,
    accountId: accountId || undefined,
  };
}

export function App(): JSX.Element {
  const client = useKMail();
  const navigate = useNavigate();

  const [mailboxes, setMailboxes] = useState<JsMailbox[]>([]);
  // `setupError` is fatal — missing session params or a failed
  // initial open(). The Sync button stays disabled while this is
  // set because no SDK session exists to sync. `syncError` is
  // transient — a failed sync() call once the SDK is open. The
  // user can retry, so the button stays enabled.
  const [setupError, setSetupError] = useState<string | null>(null);
  const [syncError, setSyncError] = useState<string | null>(null);
  const [syncing, setSyncing] = useState(false);
  const [lastSyncAt, setLastSyncAt] = useState<Date | null>(null);

  // Open the SDK once on mount. Strict mode runs effects twice
  // in dev — the IPC handler is idempotent for the same config,
  // so the second open() is a no-op.
  //
  // The `cancelled` guard prevents the state setters from firing
  // on a stale closure when the effect re-runs (StrictMode double
  // invoke, or future hot-reload). Without it, the in-flight
  // open()/cachedMailboxes() from the first invocation would race
  // the cleanup of the second, potentially overwriting freshly
  // computed state with stale data.
  useEffect(() => {
    let cancelled = false;
    const params = readSessionParams();
    if (!params) {
      setSetupError(
        'Missing session parameters — launch from KChat or set VITE_KMAIL_* env vars in dev.',
      );
      return;
    }
    void (async () => {
      try {
        await client.open({
          bffUrl: params.bffUrl,
          bearerToken: params.bearerToken,
          databasePath: params.databasePath,
          accountId: params.accountId,
        });
        if (cancelled) return;
        const m = await client.cachedMailboxes();
        if (cancelled) return;
        setMailboxes(m);
      } catch (err) {
        if (cancelled) return;
        // `KMailDesktopClient` already wraps every IPC error in
        // `parseKMailError`, so `err` is guaranteed to be a
        // `KMailError` whose `.message` already starts with the
        // tag (e.g. `[STORE] sqlite locked`). Rendering
        // `${e.tag} ${e.message}` here would double-stamp the
        // tag — see `parseKMailError` in `kmail/errors.ts` for
        // the underlying contract.
        const e = err as KMailError;
        setSetupError(e.message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client]);

  const runSync = useCallback(async () => {
    setSyncing(true);
    try {
      await client.sync();
      const m = await client.cachedMailboxes();
      setMailboxes(m);
      setLastSyncAt(new Date());
      setSyncError(null);
    } catch (err) {
      const e = err as KMailError;
      // `e.message` already includes the tag prefix; see the
      // matching comment in the mount effect above.
      setSyncError(e.message);
    } finally {
      setSyncing(false);
    }
  }, [client]);

  return (
    <div className="app-shell">
      <header className="app-header">
        <h1>KMail</h1>
        <div className="app-header-actions">
          <button
            type="button"
            onClick={() => navigate('/compose')}
            className="compose-btn"
          >
            Compose
          </button>
          <button
            type="button"
            onClick={() => void runSync()}
            // Disabled only while a sync is in flight or the SDK
            // never opened (setupError). A transient runSync
            // failure (syncError) MUST leave the button enabled
            // so the user can retry — otherwise a single failed
            // sync would lock the user out of the app.
            disabled={syncing || !!setupError}
            className="sync-btn"
          >
            {syncing ? 'Syncing…' : 'Sync'}
          </button>
          {lastSyncAt && (
            <span className="last-sync">
              Last sync {lastSyncAt.toLocaleTimeString()}
            </span>
          )}
        </div>
      </header>
      <aside className="app-sidebar">
        <h2>Mailboxes</h2>
        {mailboxes.length === 0 && !setupError && <p>Loading…</p>}
        <ul>
          {mailboxes.map((mb) => (
            <li key={mb.id}>
              <Link to={`/mailbox/${mb.id}`}>
                {mb.name}{' '}
                {mb.unreadEmails > 0n && (
                  <span className="unread-count">
                    ({mb.unreadEmails.toString()})
                  </span>
                )}
              </Link>
            </li>
          ))}
        </ul>
        {setupError && (
          <div className="error-banner" role="alert">
            <strong>SDK setup error</strong>
            <code>{setupError}</code>
          </div>
        )}
        {syncError && (
          <div className="error-banner sync-error" role="status">
            <strong>Last sync failed</strong>
            <code>{syncError}</code>
          </div>
        )}
      </aside>
      <main className="app-content">
        <Outlet />
      </main>
    </div>
  );
}
