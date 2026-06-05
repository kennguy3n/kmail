import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import type { Email } from "../../types";
import AttachmentPanel from "./AttachmentPanel";
import {
  fileAttachments,
  htmlBodyPart,
  HtmlMessageBody,
  plainBodyValue,
  useInlineImageUrls,
} from "./messageContent";

/**
 * Conversation / thread view.
 *
 * Fetches every message in a thread (ordered oldest→newest by the
 * server) and renders them as a stack of collapsible cards. The
 * newest message is expanded by default; older ones collapse to a
 * one-line summary. Each expanded message renders its HTML body
 * (sandboxed, with inline images) or plain text, plus attachments.
 */
export default function ThreadView() {
  const navigate = useNavigate();
  const { threadId } = useParams<{ threadId: string }>();
  const [emails, setEmails] = useState<Email[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!threadId) {
      setError("Missing thread id in route");
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    jmapClient
      .getThread(threadId)
      .then((list) => {
        if (cancelled) return;
        // Defensive: ensure chronological order regardless of server.
        const sorted = [...list].sort((a, b) =>
          (a.receivedAt ?? "").localeCompare(b.receivedAt ?? ""),
        );
        setEmails(sorted);
      })
      .catch((e: unknown) =>
        setError(e instanceof Error ? e.message : String(e)),
      )
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [threadId]);

  const subject = emails[0]?.subject ?? "(no subject)";

  const replyToLast = () => {
    const last = emails[emails.length - 1];
    if (!last) return;
    const target =
      last.replyTo && last.replyTo.length > 0
        ? last.replyTo
        : (last.from ?? []);
    navigate("/mail/compose", {
      state: {
        mode: "reply",
        sourceEmailId: last.id,
        to: target,
        subject: subject.toLowerCase().startsWith("re:")
          ? subject
          : `Re: ${subject}`,
        quotedBody: plainBodyValue(last),
        quotedFrom: last.from,
        quotedDate: last.receivedAt,
      },
    });
  };

  return (
    <section style={styles.root}>
      <div style={styles.topBar}>
        <Link to="/mail" style={styles.backLink}>
          ← Back to inbox
        </Link>
        {emails.length > 0 && (
          <button type="button" onClick={replyToLast} style={styles.replyButton}>
            Reply
          </button>
        )}
      </div>

      {loading && <p style={styles.muted}>Loading conversation…</p>}
      {error && <div style={styles.error}>{error}</div>}

      {!loading && !error && (
        <>
          <h1 style={styles.subject}>
            {subject}{" "}
            <span style={styles.count}>
              ({emails.length} message{emails.length === 1 ? "" : "s"})
            </span>
          </h1>
          <div style={styles.stack}>
            {emails.map((email, i) => (
              <MessageCard
                key={email.id}
                email={email}
                defaultExpanded={i === emails.length - 1}
              />
            ))}
          </div>
        </>
      )}
    </section>
  );
}

function MessageCard({
  email,
  defaultExpanded,
}: {
  email: Email;
  defaultExpanded: boolean;
}) {
  const [expanded, setExpanded] = useState(defaultExpanded);
  const cidUrls = useInlineImageUrls(expanded ? email : null);

  const html = htmlBodyPart(email);
  const plain = plainBodyValue(email);
  const files = fileAttachments(email);
  const fromLabel = formatAddresses(email.from);

  return (
    <article style={styles.card}>
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        style={styles.cardHeader}
        aria-expanded={expanded}
      >
        <span style={styles.fromName}>{fromLabel}</span>
        {!expanded && <span style={styles.previewText}>{email.preview}</span>}
        <span style={styles.date}>{formatDate(email.receivedAt)}</span>
      </button>
      {expanded && (
        <div style={styles.cardBody}>
          <dl style={styles.headerList}>
            <dt>To</dt>
            <dd>{formatAddresses(email.to)}</dd>
            {email.cc && email.cc.length > 0 && (
              <>
                <dt>Cc</dt>
                <dd>{formatAddresses(email.cc)}</dd>
              </>
            )}
          </dl>
          {html ? (
            <HtmlMessageBody
              html={email.bodyValues![html.partId!].value}
              cidUrls={cidUrls}
            />
          ) : plain ? (
            <pre style={styles.bodyPre}>{plain}</pre>
          ) : (
            <p style={styles.muted}>(empty message body)</p>
          )}
          <AttachmentPanel attachments={files} />
        </div>
      )}
    </article>
  );
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

const styles: Record<string, React.CSSProperties> = {
  root: { padding: "1rem", maxWidth: "900px" },
  topBar: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.75rem",
  },
  backLink: { color: "#2563eb", textDecoration: "none", fontSize: "0.9rem" },
  replyButton: {
    padding: "0.3rem 0.8rem",
    fontSize: "0.8rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  subject: { fontSize: "1.3rem", margin: "0 0 0.75rem" },
  count: { fontSize: "0.9rem", fontWeight: 400, color: "#6b7280" },
  stack: { display: "grid", gap: "0.5rem" },
  card: {
    border: "1px solid #e5e7eb",
    borderRadius: "0.5rem",
    background: "#fff",
    overflow: "hidden",
  },
  cardHeader: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    width: "100%",
    padding: "0.6rem 0.8rem",
    background: "#f9fafb",
    border: "none",
    borderBottom: "1px solid #f3f4f6",
    cursor: "pointer",
    textAlign: "left",
    fontSize: "0.85rem",
  },
  fromName: { fontWeight: 600, color: "#111827", flexShrink: 0 },
  previewText: {
    color: "#6b7280",
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
    flex: 1,
  },
  date: { color: "#9ca3af", marginLeft: "auto", flexShrink: 0 },
  cardBody: { padding: "0.8rem" },
  headerList: {
    display: "grid",
    gridTemplateColumns: "60px 1fr",
    rowGap: "0.2rem",
    columnGap: "0.6rem",
    margin: "0 0 0.6rem",
    fontSize: "0.82rem",
    color: "#374151",
  },
  bodyPre: {
    whiteSpace: "pre-wrap",
    fontFamily: "inherit",
    fontSize: "0.92rem",
    margin: 0,
  },
  muted: { color: "#6b7280", fontStyle: "italic" },
  error: {
    padding: "0.5rem 0.75rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
  },
};
