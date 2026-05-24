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

  const [draftsMailboxId, setDraftsMailboxId] = useState<string | null>(null);
  const [from, setFrom] = useState({ name: '', email: '' });
  const [to, setTo] = useState('');
  const [subject, setSubject] = useState('');
  const [body, setBody] = useState('');
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    void (async () => {
      try {
        const mailboxes: JsMailbox[] = await client.cachedMailboxes();
        const drafts = mailboxes.find((m) => m.role === 'drafts');
        setDraftsMailboxId(drafts?.id ?? null);
      } catch (err) {
        const e = err as KMailError;
        setError(`${e.tag} ${e.message}`);
      }
    })();
  }, [client]);

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);
    setSending(true);
    try {
      if (!draftsMailboxId) {
        setError(
          'No Drafts mailbox in the local cache — run Sync from the header first.',
        );
        setSending(false);
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
        mailboxIds: { [draftsMailboxId]: true },
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
      navigate(`/mailbox/${draftsMailboxId}`, {
        replace: true,
        state: { lastSentEmailId: id },
      });
    } catch (err) {
      const e = err as KMailError;
      setError(`${e.tag} ${e.message}`);
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
