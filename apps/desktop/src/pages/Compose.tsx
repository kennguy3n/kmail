import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useKMail } from '../kmail/client';
import type { EmailDraft } from '../kmail/client';
import { KMailError } from '../kmail/errors';
import type { JsMailbox } from '../kmail/preload';

// Compose form. Submits a real `Email/set create` JMAP call via
// the SDK's `sendEmail(...)` async method. The draft is encoded
// via `encodeWireFormatDraft` so the BFF sees byte-identical
// payloads from the iOS / Android / Desktop clients.
export function Compose(): JSX.Element {
  const client = useKMail();
  const navigate = useNavigate();

  // We cache the ids of the three mailboxes the Compose flow
  // touches: drafts (where the JMAP `Email/set create` writes the
  // initial record before submission), sent (where Stalwart moves
  // the email after EmailSubmission/set succeeds — per RFC 8621
  // §7.4), and inbox (fallback if the account is unusual and
  // doesn't expose a Sent role). All three are looked up once on
  // mount from the local cache so the send-success navigation
  // doesn't need a fresh IPC round-trip.
  const [mailboxIds, setMailboxIds] = useState<{
    drafts: string | null;
    sent: string | null;
    inbox: string | null;
  }>({ drafts: null, sent: null, inbox: null });
  const [from, setFrom] = useState({ name: '', email: '' });
  const [to, setTo] = useState('');
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    // The `cancelled` guard prevents the drafts-mailbox lookup
    // from writing state on a stale closure if the component
    // unmounts (or React StrictMode double-invokes the effect)
    // mid-flight. The cache call is local-only today, but the
    // guard future-proofs against any latency increase in
    // `cachedMailboxes()`.
    let cancelled = false;
    void (async () => {
      try {
        const mailboxes: JsMailbox[] = await client.cachedMailboxes();
        if (cancelled) return;
        // Match on JMAP canonical role names emitted by the napi
        // binding (lowercase 'drafts' / 'sent' / 'inbox'). See
        // `sdk/kmail-core/src/models.rs::MailboxRole` for the
        // canonical wire-format list.
        const findByRole = (role: string) =>
          mailboxes.find((m) => m.role === role)?.id ?? null;
        setMailboxIds({
          drafts: findByRole('drafts'),
          sent: findByRole('sent'),
          inbox: findByRole('inbox'),
        });
      } catch (err) {
        if (cancelled) return;
        // `KMailDesktopClient` already wraps every IPC error in
        // `parseKMailError`, so `e.message` already starts with
        // the tag prefix. Don't double-stamp it here.
        const e = err as KMailError;
        setError(e.message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client]);

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);
    setSending(true);
    try {
      if (!mailboxIds.drafts) {
        setError(
          'No Drafts mailbox in the local cache — run Sync from the header first.',
        );
        // The `finally` block below resets `sending`; an explicit
        // reset here would be a double-update (harmless because
        // React batches identical writes, but it muddies the
        // control flow — every `return` in this try block is
        // assumed to land in `finally`).
        return;
      }
      // Parse the To: field as a simple comma-separated list of
      // "Name <addr@example.com>" entries. A real compose UI
      // would use a chip-input control with autocomplete, but
      // the typed surface accepts both shapes.
      const toAddresses = to
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)
        .map((entry) => {
          const m = entry.match(/^(.*?)\s*<(.+?)>\s*$/);
          if (m) return { name: m[1].trim(), email: m[2].trim() };
          return { name: '', email: entry };
        });

      const draft: EmailDraft = {
        mailboxIds: { [mailboxIds.drafts]: true },
        from: [from],
        to: toAddresses,
        subject,
        textBody: body || undefined,
      };
      const id = await client.sendDraft(draft);
      void client.notify(
        'Email sent',
        subject ? `Subject: ${subject}` : 'Your message is on its way.',
      );
      // Post-send navigation: prefer Sent (where Stalwart routes
      // the email after EmailSubmission/set), then Inbox, then
      // Drafts as a last resort. Landing the user on Drafts after
      // a successful send is confusing UX — the email is no
      // longer there. The fallback chain handles JMAP accounts
      // where Sent doesn't exist as a separate role (rare, but
      // legal per RFC 8621 §2.5 — `role` is optional and the
      // server may collapse Sent into All-mail).
      const target =
        mailboxIds.sent ?? mailboxIds.inbox ?? mailboxIds.drafts;
      navigate(`/mailbox/${target}`, {
        replace: true,
        state: { lastSentEmailId: id },
      });
    } catch (err) {
      const e = err as KMailError;
      // `e.message` already contains the tag prefix; see the
      // matching comment in the mount effect above.
      setError(e.message);
    } finally {
      setSending(false);
    }
  };

  return (
    <form className="compose-form" onSubmit={(e) => void handleSubmit(e)}>
      <h2>New message</h2>
      <label>
        From (display name)
        <input
          value={from.name}
          onChange={(e) => setFrom({ ...from, name: e.target.value })}
          placeholder="Your name"
        />
      </label>
      <label>
        From (email)
        <input
          type="email"
          required
          value={from.email}
          onChange={(e) => setFrom({ ...from, email: e.target.value })}
          placeholder="you@example.com"
        />
      </label>
      <label>
        To (comma-separated)
        <input
          required
          value={to}
          onChange={(e) => setTo(e.target.value)}
          placeholder='Alice <alice@example.com>, bob@example.com'
        />
      </label>
      <label>
        Subject
        <input
          value={subject}
          onChange={(e) => setSubject(e.target.value)}
        />
      </label>
      <label>
        Message
        <textarea
          value={body}
          onChange={(e) => setBody(e.target.value)}
          rows={12}
        />
      </label>
      {error && (
        <div className="error-banner" role="alert">
          <code>{error}</code>
        </div>
      )}
      <div className="compose-actions">
        <button type="submit" disabled={sending}>
          {sending ? 'Sending…' : 'Send'}
        </button>
        <button
          type="button"
          onClick={() => navigate(-1)}
          disabled={sending}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
