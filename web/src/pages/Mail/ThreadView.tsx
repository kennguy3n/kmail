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
    <section className={styles.root}>
      <div className={styles.topBar}>
        <Link to="/mail" className={styles.backLink}>
          ← Back to inbox
        </Link>
        {emails.length > 0 && (
          <button type="button" onClick={replyToLast} className={styles.replyButton}>
            Reply
          </button>
        )}
      </div>

      {loading && <p className={styles.muted}>Loading conversation…</p>}
      {error && <div className={styles.error}>{error}</div>}

      {!loading && !error && (
        <>
          <h1 className={styles.subject}>
            {subject}{" "}
            <span className={styles.count}>
              ({emails.length} message{emails.length === 1 ? "" : "s"})
            </span>
          </h1>
          <div className={styles.stack}>
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
    <article className={styles.card}>
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        className={styles.cardHeader}
        aria-expanded={expanded}
      >
        <span className={styles.fromName}>{fromLabel}</span>
        {!expanded && <span className={styles.previewText}>{email.preview}</span>}
        <span className={styles.date}>{formatDate(email.receivedAt)}</span>
      </button>
      {expanded && (
        <div className={styles.cardBody}>
          <dl className={styles.headerList}>
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
            <pre className={styles.bodyPre}>{plain}</pre>
          ) : (
            <p className={styles.muted}>(empty message body)</p>
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

/** Theme-aware Tailwind class recipes for the conversation ThreadView. */
const styles: Record<string, string> = {
  root: "max-w-[900px] p-4",
  topBar: "mb-3 flex items-center justify-between",
  backLink: "text-sm text-primary no-underline hover:underline",
  replyButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3 py-1.5 text-xs font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  subject: "mb-3 mt-0 text-xl font-semibold",
  count: "text-sm font-normal text-fg-muted",
  stack: "grid gap-2",
  card: "overflow-hidden rounded-lg border border-border bg-surface",
  cardHeader:
    "flex w-full cursor-pointer items-center gap-2 border-0 border-b border-border bg-surface-muted px-3 py-2.5 text-left text-sm",
  fromName: "shrink-0 font-semibold text-fg",
  previewText: "flex-1 overflow-hidden text-ellipsis whitespace-nowrap text-fg-muted",
  date: "ml-auto shrink-0 text-fg-subtle",
  cardBody: "p-3",
  headerList:
    "mb-2.5 grid grid-cols-[60px_1fr] gap-x-2.5 gap-y-1 text-xs text-fg-muted",
  bodyPre: "m-0 whitespace-pre-wrap font-[inherit] text-sm",
  muted: "italic text-fg-muted",
  error: "rounded-md bg-danger-bg px-3 py-2 text-danger-fg",
};
