import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import type { Email, EmailBodyPart } from "../../types";

/**
 * MessageView is the single-message reading pane.
 *
 * Fetches one Email via `Email/get` with a full property set. For
 * Vault mailboxes the BFF currently returns plaintext (Phase 2);
 * client-side MLS decryption of StrictZK blobs
 * (docs/JMAP-CONTRACT.md §2.4) is deferred to Phase 3.
 */
export default function MessageView() {
  const navigate = useNavigate();
  const { mailboxId, emailId } = useParams<{
    mailboxId: string;
    emailId: string;
  }>();
  const [email, setEmail] = useState<Email | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [isLoading, setLoading] = useState(true);

  useEffect(() => {
    if (!emailId) {
      setError("Missing email id in route");
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setEmail(null);
    setError(null);
    jmapClient
      .getEmail(emailId)
      .then((e) => {
        if (cancelled) return;
        setEmail(e);
        // Mark-on-open: only call if the message is currently unread.
        // We intentionally fire-and-forget — a failure here shouldn't
        // block rendering the message body the user is already reading.
        if (!e.keywords.$seen) {
          jmapClient.markRead(e.id, true).catch(() => {
            // Swallow; surfacing this would be more noisy than useful.
          });
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : "Unknown error");
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [emailId]);

  const bodyText = useMemo(() => resolveBody(email), [email]);
  const attachments = useMemo(() => resolveAttachments(email), [email]);

  const handleReply = (replyAll: boolean) => {
    if (!email) return;
    navigate("/mail/compose", {
      state: {
        mode: replyAll ? "replyAll" : "reply",
        sourceEmailId: email.id,
        to: email.from ?? [],
        cc: replyAll ? [...(email.to ?? []), ...(email.cc ?? [])] : [],
        subject: withPrefix(email.subject, "Re:"),
        quotedBody: bodyText,
        quotedFrom: email.from,
        quotedDate: email.receivedAt,
      },
    });
  };

  const handleForward = () => {
    if (!email) return;
    navigate("/mail/compose", {
      state: {
        mode: "forward",
        sourceEmailId: email.id,
        subject: withPrefix(email.subject, "Fwd:"),
        quotedBody: bodyText,
        quotedFrom: email.from,
        quotedDate: email.receivedAt,
      },
    });
  };

  return (
    <section style={viewStyles.root}>
      <div style={viewStyles.topBar}>
        <Link
          to={mailboxId ? `/mail/${mailboxId}` : "/mail"}
          style={viewStyles.backLink}
        >
          ← Back to inbox
        </Link>
      </div>
      {isLoading && <p style={viewStyles.muted}>Loading message…</p>}
      {error && <div style={viewStyles.error}>{error}</div>}
      {email && (
        <article style={viewStyles.article}>
          <header style={viewStyles.header}>
            <div style={viewStyles.subjectRow}>
              <h1 style={viewStyles.subject}>
                {email.subject ?? "(no subject)"}
              </h1>
              <div style={viewStyles.actions}>
                <button
                  type="button"
                  onClick={() => handleReply(false)}
                  style={viewStyles.actionButton}
                >
                  Reply
                </button>
                <button
                  type="button"
                  onClick={() => handleReply(true)}
                  style={viewStyles.actionButton}
                >
                  Reply all
                </button>
                <button
                  type="button"
                  onClick={handleForward}
                  style={viewStyles.actionButton}
                >
                  Forward
                </button>
              </div>
            </div>
            <dl style={viewStyles.headerList}>
              <dt>From</dt>
              <dd>{formatAddresses(email.from)}</dd>
              <dt>To</dt>
              <dd>{formatAddresses(email.to)}</dd>
              {email.cc && email.cc.length > 0 && (
                <>
                  <dt>Cc</dt>
                  <dd>{formatAddresses(email.cc)}</dd>
                </>
              )}
              <dt>Date</dt>
              <dd>{formatDate(email.receivedAt)}</dd>
              {email.privacyMode && (
                <>
                  <dt>Privacy</dt>
                  <dd>{email.privacyMode}</dd>
                </>
              )}
            </dl>
          </header>
          <div style={viewStyles.body}>
            {bodyText ? (
              <pre style={viewStyles.bodyPre}>{bodyText}</pre>
            ) : (
              <p style={viewStyles.muted}>(empty message body)</p>
            )}
          </div>
          {attachments.length > 0 && (
            <section style={viewStyles.attachmentsBox}>
              <h2 style={viewStyles.attachmentsTitle}>
                Attachments ({attachments.length})
              </h2>
              <ul style={viewStyles.attachmentsList}>
                {attachments.map((a) => (
                  <li key={a.partId ?? a.name ?? Math.random()}>
                    <span style={viewStyles.attachmentName}>
                      {a.name ?? "(unnamed)"}
                    </span>
                    <span style={viewStyles.attachmentMeta}>
                      {formatType(a.type)}
                      {typeof a.size === "number"
                        ? ` · ${formatBytes(a.size)}`
                        : ""}
                    </span>
                  </li>
                ))}
              </ul>
            </section>
          )}
        </article>
      )}
    </section>
  );
}

/**
 * Extract a displayable body string from the Email. Prefers the
 * first text/plain part; falls back to stripping tags from the
 * first text/html part so the reading pane at least shows
 * something. A real client will render HTML in a sandboxed iframe,
 * but Phase 2 keeps it simple.
 */
function resolveBody(email: Email | null): string {
  if (!email || !email.bodyValues) return "";
  const textPart = email.textBody?.find(isPart);
  if (textPart?.partId && email.bodyValues[textPart.partId]) {
    return email.bodyValues[textPart.partId].value;
  }
  const htmlPart = email.htmlBody?.find(isPart);
  if (htmlPart?.partId && email.bodyValues[htmlPart.partId]) {
    return stripHtml(email.bodyValues[htmlPart.partId].value);
  }
  return "";
}

/**
 * Return the parts of an email that should be listed as
 * attachments. `attachments` is the canonical JMAP field (RFC 8621
 * §4.1.4); we fall back to body parts that set a `disposition` of
 * `attachment` when the server does not populate it.
 */
function resolveAttachments(email: Email | null): EmailBodyPart[] {
  if (!email) return [];
  if (email.attachments && email.attachments.length > 0) {
    return email.attachments;
  }
  const fromBody = (email.bodyStructure?.subParts ?? [])
    .concat(email.bodyStructure ? [email.bodyStructure] : [])
    .filter((p) => p.disposition === "attachment");
  return fromBody;
}

function withPrefix(
  subject: string | null | undefined,
  prefix: string,
): string {
  const trimmed = (subject ?? "").trim();
  if (!trimmed) return prefix;
  if (trimmed.toLowerCase().startsWith(prefix.toLowerCase())) {
    return trimmed;
  }
  return `${prefix} ${trimmed}`;
}

function formatType(type: string | null | undefined): string {
  if (!type) return "application/octet-stream";
  return type;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function isPart(part: EmailBodyPart): boolean {
  return Boolean(part.partId);
}

function stripHtml(input: string): string {
  return input
    .replace(/<style[\s\S]*?<\/style>/gi, "")
    .replace(/<script[\s\S]*?<\/script>/gi, "")
    .replace(/<[^>]+>/g, "")
    .replace(/&nbsp;/g, " ")
    .replace(/\s+\n/g, "\n")
    .trim();
}

function formatAddresses(
  list: { name: string | null; email: string }[] | null | undefined,
): string {
  if (!list || list.length === 0) return "(none)";
  return list
    .map((a) => (a.name ? `${a.name} <${a.email}>` : a.email))
    .join(", ");
}

function formatDate(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

const viewStyles: Record<string, React.CSSProperties> = {
  root: {
    padding: "1rem",
    maxWidth: "900px",
  },
  topBar: {
    marginBottom: "0.75rem",
  },
  backLink: {
    color: "#2563eb",
    textDecoration: "none",
    fontSize: "0.9rem",
  },
  article: {
    border: "1px solid #e5e7eb",
    borderRadius: "0.5rem",
    padding: "1.25rem",
    background: "#fff",
  },
  header: {
    borderBottom: "1px solid #e5e7eb",
    paddingBottom: "0.75rem",
    marginBottom: "1rem",
  },
  subjectRow: {
    display: "flex",
    alignItems: "flex-start",
    justifyContent: "space-between",
    gap: "0.75rem",
    marginBottom: "0.75rem",
  },
  subject: {
    margin: 0,
    fontSize: "1.25rem",
  },
  actions: {
    display: "flex",
    gap: "0.25rem",
    flexShrink: 0,
  },
  actionButton: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
  },
  attachmentsBox: {
    marginTop: "1rem",
    padding: "0.75rem",
    borderTop: "1px solid #e5e7eb",
  },
  attachmentsTitle: {
    margin: "0 0 0.5rem",
    fontSize: "0.95rem",
  },
  attachmentsList: {
    listStyle: "none",
    margin: 0,
    padding: 0,
    display: "flex",
    flexDirection: "column",
    gap: "0.25rem",
    fontSize: "0.85rem",
  },
  attachmentName: {
    marginRight: "0.5rem",
  },
  attachmentMeta: {
    color: "#6b7280",
  },
  headerList: {
    display: "grid",
    gridTemplateColumns: "80px 1fr",
    rowGap: "0.25rem",
    columnGap: "0.75rem",
    margin: 0,
    fontSize: "0.9rem",
  },
  body: {
    lineHeight: 1.5,
  },
  bodyPre: {
    whiteSpace: "pre-wrap",
    fontFamily: "inherit",
    fontSize: "0.95rem",
    margin: 0,
  },
  error: {
    padding: "0.5rem 0.75rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
  },
  muted: {
    color: "#6b7280",
    fontStyle: "italic",
  },
};
