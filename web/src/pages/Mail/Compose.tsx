import {
  FormEvent,
  useEffect,
  useMemo,
  useState,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import type {
  EmailAddress,
  EmailDraft,
  Identity,
  Mailbox,
  PrivacyMode,
} from "../../types";

/**
 * Compose is the message composition view.
 *
 * Flow:
 * 1. On mount, fetch mailboxes (to find Drafts) and identities (to
 *    pick the `From`).
 * 2. Seed the form from route state when the user arrived via a
 *    Reply / Reply-All / Forward button on `MessageView`.
 * 3. On Send, call `jmapClient.sendEmail()` which batches
 *    `Email/set create` + `EmailSubmission/set` and returns the
 *    created Email id. The BFF resolves the Identity id and does
 *    the `onSuccessDestroyEmail` drafts cleanup.
 * 4. On Save draft, call `jmapClient.createDraft()` so the message
 *    lands in the Drafts mailbox without being submitted.
 *
 * Blob/upload for attachments is deferred to Phase 3 — see
 * docs/JMAP-CONTRACT.md §4.2. For now the compose page only
 * supports inline text/html bodies.
 */
export default function Compose() {
  const navigate = useNavigate();
  const location = useLocation();
  const seed = (location.state as ComposeSeed | null) ?? null;

  const [mailboxes, setMailboxes] = useState<Mailbox[] | null>(null);
  const [identities, setIdentities] = useState<Identity[] | null>(null);
  const [to, setTo] = useState(addressesToInput(seed?.to));
  const [cc, setCc] = useState(addressesToInput(seed?.cc));
  const [bcc, setBcc] = useState(addressesToInput(seed?.bcc));
  const [subject, setSubject] = useState(seed?.subject ?? "");
  const [body, setBody] = useState(initialBody(seed));
  const [privacyMode, setPrivacyMode] = useState<PrivacyMode>("standard");
  const [selectedIdentityId, setSelectedIdentityId] = useState<string>("");
  const [isSending, setSending] = useState(false);
  const [isSavingDraft, setSavingDraft] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    Promise.all([jmapClient.getMailboxes(), jmapClient.getIdentities()])
      .then(([mbxList, idList]) => {
        if (cancelled) return;
        setMailboxes(mbxList);
        setIdentities(idList);
        if (idList.length > 0) {
          setSelectedIdentityId((current) => current || idList[0].id);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const draftsMailbox = useMemo(
    () => (mailboxes ?? []).find((m) => m.role === "drafts") ?? null,
    [mailboxes],
  );

  const identity = useMemo(
    () =>
      (identities ?? []).find((i) => i.id === selectedIdentityId) ??
      (identities ?? [])[0] ??
      null,
    [identities, selectedIdentityId],
  );

  const canSubmit =
    !!draftsMailbox &&
    !!identity &&
    to.trim().length > 0 &&
    !isSending &&
    !isSavingDraft;

  /**
   * Build the draft payload. `requireTo` defaults to `true` because
   * sending without a recipient is an error; Save draft passes
   * `false` so the user can stash work-in-progress messages before
   * they've filled in the To field.
   */
  const buildDraft = (requireTo = true): EmailDraft | null => {
    if (!draftsMailbox || !identity) return null;
    const toList = parseAddresses(to);
    if (requireTo && toList.length === 0) return null;
    return {
      mailboxIds: { [draftsMailbox.id]: true },
      from: [{ name: identity.name || null, email: identity.email }],
      to: toList,
      cc: parseAddresses(cc),
      bcc: parseAddresses(bcc),
      subject: subject.trim(),
      textBody: body,
      privacyMode,
      identityId: identity.id,
    };
  };

  const handleSend = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setSuccessMessage(null);
    const draft = buildDraft(true);
    if (!draft) {
      setError("Please supply at least one recipient and a sender identity.");
      return;
    }
    setSending(true);
    try {
      await jmapClient.sendEmail(draft);
      setSuccessMessage("Message sent.");
      // Give the user a brief moment to see the success confirmation
      // before we navigate them back to the inbox. We deliberately
      // leave `isSending` true so the Send button stays disabled
      // through the navigation delay — resetting it here would let
      // a rapid second click dispatch a duplicate submission.
      setTimeout(() => navigate("/mail"), 600);
    } catch (err: unknown) {
      setError(errorMessage(err));
      setSending(false);
    }
  };

  const handleSaveDraft = async () => {
    setError(null);
    setSuccessMessage(null);
    const draft = buildDraft(false);
    if (!draft) {
      setError("Drafts mailbox or sender identity is not yet available.");
      return;
    }
    setSavingDraft(true);
    try {
      await jmapClient.createDraft(draft);
      setSuccessMessage("Draft saved.");
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSavingDraft(false);
    }
  };

  const heading =
    seed?.mode === "reply" || seed?.mode === "replyAll"
      ? "Reply"
      : seed?.mode === "forward"
        ? "Forward"
        : "New message";

  return (
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>{heading}</h2>
      </header>
      {error && (
        <div style={styles.error} role="alert">
          <span>{error}</span>
          <button
            type="button"
            onClick={() => setError(null)}
            style={styles.errorDismiss}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {successMessage && (
        <div style={styles.success} role="status">
          {successMessage}
        </div>
      )}
      <form onSubmit={handleSend} style={styles.form}>
        <div style={styles.row}>
          <label htmlFor="compose-from" style={styles.label}>
            From
          </label>
          <select
            id="compose-from"
            value={selectedIdentityId}
            onChange={(e) => setSelectedIdentityId(e.target.value)}
            style={styles.select}
            disabled={!identities || identities.length === 0}
          >
            {(identities ?? []).length === 0 ? (
              <option value="">(loading identities…)</option>
            ) : (
              (identities ?? []).map((id) => (
                <option key={id.id} value={id.id}>
                  {id.name ? `${id.name} <${id.email}>` : id.email}
                </option>
              ))
            )}
          </select>
        </div>
        <div style={styles.row}>
          <label htmlFor="compose-to" style={styles.label}>
            To
          </label>
          <input
            id="compose-to"
            type="text"
            value={to}
            onChange={(e) => setTo(e.target.value)}
            placeholder="name@example.com, other@example.com"
            style={styles.input}
            required
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="compose-cc" style={styles.label}>
            Cc
          </label>
          <input
            id="compose-cc"
            type="text"
            value={cc}
            onChange={(e) => setCc(e.target.value)}
            style={styles.input}
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="compose-bcc" style={styles.label}>
            Bcc
          </label>
          <input
            id="compose-bcc"
            type="text"
            value={bcc}
            onChange={(e) => setBcc(e.target.value)}
            style={styles.input}
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="compose-subject" style={styles.label}>
            Subject
          </label>
          <input
            id="compose-subject"
            type="text"
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            style={styles.input}
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="compose-privacy" style={styles.label}>
            Privacy
          </label>
          <select
            id="compose-privacy"
            value={privacyMode}
            onChange={(e) => setPrivacyMode(e.target.value as PrivacyMode)}
            style={styles.select}
          >
            <option value="standard">Standard Private Mail</option>
            <option value="confidential-send">Confidential Send</option>
            <option value="zero-access-vault">Zero-Access Vault</option>
          </select>
        </div>
        <div style={styles.bodyRow}>
          <textarea
            aria-label="Message body"
            value={body}
            onChange={(e) => setBody(e.target.value)}
            placeholder="Write your message…"
            style={styles.textarea}
            rows={16}
          />
        </div>
        <div style={styles.buttonRow}>
          <button
            type="submit"
            disabled={!canSubmit}
            style={{
              ...styles.primaryButton,
              opacity: canSubmit ? 1 : 0.6,
              cursor: canSubmit ? "pointer" : "not-allowed",
            }}
          >
            {isSending ? "Sending…" : "Send"}
          </button>
          <button
            type="button"
            onClick={handleSaveDraft}
            disabled={isSending || isSavingDraft || !draftsMailbox}
            style={styles.secondaryButton}
          >
            {isSavingDraft ? "Saving…" : "Save draft"}
          </button>
          <button
            type="button"
            onClick={() => navigate(-1)}
            style={styles.secondaryButton}
            disabled={isSending}
          >
            Cancel
          </button>
        </div>
      </form>
    </section>
  );
}

/**
 * Seed shape passed by `MessageView` when the user clicks
 * Reply / Reply All / Forward. Kept deliberately loose so future
 * entry points (draft editor, open URL-encoded mailto link) can
 * reuse it without schema churn.
 */
interface ComposeSeed {
  mode?: "reply" | "replyAll" | "forward";
  sourceEmailId?: string;
  to?: EmailAddress[];
  cc?: EmailAddress[];
  bcc?: EmailAddress[];
  subject?: string;
  quotedBody?: string;
  quotedFrom?: EmailAddress[] | null;
  quotedDate?: string | null;
}

function initialBody(seed: ComposeSeed | null): string {
  if (!seed || !seed.quotedBody) return "";
  const header = buildQuoteHeader(seed);
  const quoted = seed.quotedBody
    .split("\n")
    .map((line) => `> ${line}`)
    .join("\n");
  return `\n\n${header}\n${quoted}\n`;
}

function buildQuoteHeader(seed: ComposeSeed): string {
  const who =
    seed.quotedFrom && seed.quotedFrom.length > 0
      ? seed.quotedFrom
          .map((a) => (a.name ? `${a.name} <${a.email}>` : a.email))
          .join(", ")
      : "(unknown sender)";
  const when = seed.quotedDate
    ? new Date(seed.quotedDate).toLocaleString()
    : "(unknown date)";
  return `On ${when}, ${who} wrote:`;
}

/**
 * Serialise an address list for the comma-separated text inputs.
 * Display names that contain a comma or a double-quote are wrapped
 * in double quotes (with embedded quotes backslash-escaped) so the
 * round-trip through `parseAddresses` doesn't corrupt them — e.g.
 * `{ name: "Smith, John", email: "j@x" }` round-trips as
 * `"Smith, John" <j@x>` instead of splitting into two entries.
 */
function addressesToInput(list: EmailAddress[] | undefined): string {
  if (!list || list.length === 0) return "";
  return list.map((a) => formatAddress(a)).join(", ");
}

function formatAddress(a: EmailAddress): string {
  if (!a.name) return a.email;
  const needsQuoting = /[,"<>]/.test(a.name);
  const name = needsQuoting
    ? `"${a.name.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`
    : a.name;
  return `${name} <${a.email}>`;
}

/**
 * Parse a comma-separated list of addresses. Accepts bare
 * `user@host`, `Display Name <user@host>`, and
 * `"Quoted, Name" <user@host>` forms. Commas inside balanced
 * double quotes do NOT split entries. Blank entries are silently
 * dropped; malformed entries fall through as
 * `{ name: null, email: <raw> }` so the server can return a
 * JMAP-level `invalidProperties` error rather than us guessing.
 */
function parseAddresses(input: string): EmailAddress[] {
  return splitOnTopLevelCommas(input)
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
    .map((s) => {
      const match = s.match(/^(.*)<\s*([^>]+)\s*>\s*$/);
      if (match) {
        const rawName = match[1].trim();
        const name = unquoteName(rawName) || null;
        return { name, email: match[2].trim() };
      }
      return { name: null, email: s };
    });
}

/**
 * Split on commas that are NOT inside a double-quoted segment.
 * Handles backslash-escaped quotes within the quoted segment.
 */
function splitOnTopLevelCommas(input: string): string[] {
  const out: string[] = [];
  let current = "";
  let inQuotes = false;
  for (let i = 0; i < input.length; i++) {
    const ch = input[i];
    if (ch === "\\" && inQuotes && i + 1 < input.length) {
      current += ch + input[i + 1];
      i++;
      continue;
    }
    if (ch === '"') {
      inQuotes = !inQuotes;
      current += ch;
      continue;
    }
    if (ch === "," && !inQuotes) {
      out.push(current);
      current = "";
      continue;
    }
    current += ch;
  }
  if (current.length > 0) out.push(current);
  return out;
}

function unquoteName(raw: string): string {
  const trimmed = raw.trim();
  if (trimmed.length >= 2 && trimmed.startsWith('"') && trimmed.endsWith('"')) {
    return trimmed
      .slice(1, -1)
      .replace(/\\"/g, '"')
      .replace(/\\\\/g, "\\");
  }
  return trimmed;
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Unknown error";
}

const styles: Record<string, React.CSSProperties> = {
  root: {
    padding: "1rem",
    maxWidth: "900px",
  },
  header: {
    marginBottom: "0.75rem",
  },
  title: {
    margin: 0,
    fontSize: "1.25rem",
  },
  form: {
    display: "flex",
    flexDirection: "column",
    gap: "0.5rem",
    border: "1px solid #e5e7eb",
    borderRadius: "0.5rem",
    padding: "1rem",
    background: "#fff",
  },
  row: {
    display: "grid",
    gridTemplateColumns: "80px 1fr",
    alignItems: "center",
    gap: "0.5rem",
  },
  bodyRow: {
    display: "flex",
    flexDirection: "column",
    marginTop: "0.25rem",
  },
  label: {
    fontSize: "0.85rem",
    color: "#374151",
    fontWeight: 600,
  },
  input: {
    padding: "0.4rem 0.5rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  select: {
    padding: "0.4rem 0.5rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    background: "#fff",
  },
  textarea: {
    padding: "0.6rem",
    fontSize: "0.9rem",
    fontFamily: "inherit",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    resize: "vertical",
    minHeight: "16rem",
  },
  buttonRow: {
    display: "flex",
    gap: "0.5rem",
    marginTop: "0.5rem",
  },
  primaryButton: {
    padding: "0.5rem 1rem",
    fontSize: "0.9rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
  },
  secondaryButton: {
    padding: "0.5rem 1rem",
    fontSize: "0.9rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
  },
  error: {
    padding: "0.5rem 0.75rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    marginBottom: "0.5rem",
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: "0.5rem",
  },
  errorDismiss: {
    background: "transparent",
    border: "none",
    color: "#991b1b",
    fontSize: "1.1rem",
    cursor: "pointer",
    lineHeight: 1,
    padding: "0 0.25rem",
  },
  success: {
    padding: "0.5rem 0.75rem",
    background: "#dcfce7",
    color: "#166534",
    borderRadius: "0.25rem",
    marginBottom: "0.5rem",
  },
};
