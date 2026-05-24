import { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';
import { useKMail } from '../kmail/client';
import { KMailError } from '../kmail/errors';
import type { JsEmailSummary } from '../kmail/preload';

const DEFAULT_LIMIT = 50;

// Email list for a given mailbox.
//
// Reads from the local SQLite cache via `cachedEmails(...)`. The
// list is always served from cache — if the cache is stale the
// user can hit the Sync button in the app header. This matches
// the offline-first design contract documented in
// `docs/SDK.md` §3 (offline-first delta-pull).
export function Inbox(): JSX.Element {
  const { mailboxId } = useParams<{ mailboxId?: string }>();
  const client = useKMail();
  const [emails, setEmails] = useState<JsEmailSummary[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<JsEmailSummary | null>(null);

  useEffect(() => {
    if (!mailboxId) {
      setEmails([]);
      return;
    }
    // The `cancelled` guard prevents stale closures when the user
    // navigates between mailboxes faster than the local SQLite
    // cache can respond. Without it, a resolved fetch from
    // mailbox A could overwrite the freshly-rendered list for
    // mailbox B. Cache reads are sub-millisecond today, but the
    // guard future-proofs the component against any IPC-level
    // latency increase (e.g. if the cached path ever wraps an
    // SDK call that hits the JMAP server).
    let cancelled = false;
    void (async () => {
      try {
        const list = await client.cachedEmails(mailboxId, DEFAULT_LIMIT);
        if (cancelled) return;
        setEmails(list);
        setSelected(list[0] ?? null);
        setError(null);
      } catch (err) {
        if (cancelled) return;
        // `KMailDesktopClient` already wraps every IPC error in
        // `parseKMailError`, so `e.message` already starts with
        // the tag prefix. Don't prepend `e.tag` here or it gets
        // double-stamped.
        const e = err as KMailError;
        setError(e.message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, mailboxId]);

  if (!mailboxId) {
    return (
      <div className="inbox-empty">
        <p>Select a mailbox from the sidebar.</p>
      </div>
    );
  }

  return (
    <div className="inbox">
      <section className="email-list" aria-label="Email list">
        {error && (
          <div className="error-banner" role="alert">
            <code>{error}</code>
          </div>
        )}
        {emails.length === 0 && !error && <p>No emails in this mailbox.</p>}
        <ul>
          {emails.map((email) => (
            <li
              key={email.id}
              className={
                selected?.id === email.id ? 'email-row selected' : 'email-row'
              }
              onClick={() => setSelected(email)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' || e.key === ' ') setSelected(email);
              }}
              role="button"
              tabIndex={0}
            >
              <div className="email-from">
                {email.fromAddresses
                  .map((a) => a.name || a.email)
                  .join(', ') || '(unknown sender)'}
              </div>
              <div className="email-subject">
                {email.subject || '(no subject)'}
              </div>
              <div className="email-preview">{email.preview}</div>
            </li>
          ))}
        </ul>
      </section>
      <section className="email-detail" aria-label="Email detail">
        {selected ? (
          <article>
            <header>
              <h2>{selected.subject || '(no subject)'}</h2>
              <div className="email-meta">
                From:{' '}
                {selected.fromAddresses
                  .map((a) => `${a.name} <${a.email}>`)
                  .join(', ')}
                <br />
                To:{' '}
                {selected.toAddresses
                  .map((a) => `${a.name} <${a.email}>`)
                  .join(', ')}
                <br />
                Received:{' '}
                {new Date(
                  selected.receivedAtUnix * 1000,
                ).toLocaleString()}
              </div>
            </header>
            <p className="email-body">{selected.preview}</p>
            <footer>
              <em>
                Full body fetch lands in a follow-up PR — the desktop SDK
                only caches metadata + preview today.
              </em>
            </footer>
          </article>
        ) : (
          <p>Select an email to read.</p>
        )}
      </section>
    </div>
  );
}
