import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import { snoozeEmail } from "../../api/snooze";
import {
  getSmartReplies,
  getUnsubscribe,
  postUnsubscribe,
  type SmartReplySuggestion,
  type UnsubscribeInfoResponse,
} from "../../api/smart";
import SnoozePicker from "./SnoozePicker";
import AttachmentPanel from "./AttachmentPanel";
import {
  fileAttachments,
  htmlBodyPart,
  HtmlMessageBody,
  useInlineImageUrls,
} from "./messageContent";
import ReadReceiptPrompt from "./ReadReceiptPrompt";
import type { Email, EmailAddress, EmailBodyPart } from "../../types";

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
  const [snoozePickerOpen, setSnoozePickerOpen] = useState(false);
  const [snoozeBusy, setSnoozeBusy] = useState(false);
  const [snoozeConfirmation, setSnoozeConfirmation] = useState<string | null>(
    null,
  );

  // WS7: smart reply suggestions
  const [smartReplies, setSmartReplies] = useState<SmartReplySuggestion[]>([]);
  const [unsubInfo, setUnsubInfo] = useState<UnsubscribeInfoResponse | null>(null);
  const [unsubBusy, setUnsubBusy] = useState(false);

  useEffect(() => {
    if (!emailId) return;
    getSmartReplies(emailId)
      .then((r) => setSmartReplies(r.suggestions ?? []))
      .catch(() => { /* best-effort */ });
    getUnsubscribe(emailId)
      .then(setUnsubInfo)
      .catch(() => { /* best-effort */ });
  }, [emailId]);

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
  const htmlPart = useMemo(() => (email ? htmlBodyPart(email) : null), [email]);
  const attachments = useMemo(
    () => (email ? fileAttachments(email) : []),
    [email],
  );
  const cidUrls = useInlineImageUrls(email);

  // Prefer the Reply-To header when present (mailing lists, shared
  // inboxes, newsletters) over the From address. Shared by the
  // Reply buttons and the smart-reply chips so both target the same
  // address.
  const replyTarget = useMemo<EmailAddress[]>(
    () =>
      email?.replyTo && email.replyTo.length > 0
        ? email.replyTo
        : (email?.from ?? []),
    [email],
  );

  const handleReply = (replyAll: boolean) => {
    if (!email) return;
    // For Reply-All, dedupe the CC list against the reply target so
    // the same address doesn't end up in both To and Cc. Compose
    // does a second pass to strip the sender's own identity, which
    // is not available here.
    const replyAllCc = replyAll
      ? dedupeAddresses(
          [...(email.to ?? []), ...(email.cc ?? [])],
          replyTarget,
        )
      : [];
    navigate("/mail/compose", {
      state: {
        mode: replyAll ? "replyAll" : "reply",
        sourceEmailId: email.id,
        to: replyTarget,
        cc: replyAllCc,
        subject: withPrefix(email.subject, "Re:"),
        quotedBody: bodyText,
        quotedFrom: email.from,
        quotedDate: email.receivedAt,
      },
    });
  };

  const handleSnooze = async (until: Date) => {
    if (!email || snoozeBusy) return;
    setSnoozeBusy(true);
    try {
      // Centralised lookup-or-create. The helper fetches the
      // LIVE mailbox list and recovers from concurrent-create
      // races so Inbox and MessageView can't double-create the
      // Snoozed mailbox from different views.
      const snoozedId = await jmapClient.resolveOrCreateSnoozedMailbox();
      const originals = { ...email.mailboxIds } as Record<string, boolean>;
      if (originals[snoozedId]) {
        throw new Error(
          "Email is already in the Snoozed mailbox — wake it first.",
        );
      }
      await snoozeEmail({
        email_id: email.id,
        original_mailbox_ids: originals,
        snoozed_mailbox_id: snoozedId,
        snooze_until: until.toISOString(),
        mark_unread_on_wake: true,
      });
      setSnoozePickerOpen(false);
      setSnoozeConfirmation(
        `Snoozed until ${until.toLocaleString()}. The message will return to your inbox then.`,
      );
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Unknown error");
    } finally {
      setSnoozeBusy(false);
    }
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
    <section className={viewStyles.root}>
      <div className={viewStyles.topBar}>
        <Link
          to={mailboxId ? `/mail/${mailboxId}` : "/mail"}
          className={viewStyles.backLink}
        >
          ← Back to inbox
        </Link>
      </div>
      {isLoading && <p className={viewStyles.muted}>Loading message…</p>}
      {error && <div className={viewStyles.error}>{error}</div>}
      {snoozeConfirmation && (
        <div className={viewStyles.snoozeConfirmation}>{snoozeConfirmation}</div>
      )}
      {email && (
        <article className={viewStyles.article}>
          <header className={viewStyles.header}>
            <div className={viewStyles.subjectRow}>
              <h1 className={viewStyles.subject}>
                {email.subject ?? "(no subject)"}
              </h1>
              <div className={viewStyles.actions}>
                <button
                  type="button"
                  onClick={() => handleReply(false)}
                  className={viewStyles.actionButton}
                >
                  Reply
                </button>
                <button
                  type="button"
                  onClick={() => handleReply(true)}
                  className={viewStyles.actionButton}
                >
                  Reply all
                </button>
                <button
                  type="button"
                  onClick={handleForward}
                  className={viewStyles.actionButton}
                >
                  Forward
                </button>
                <div className={viewStyles.snoozeWrap}>
                  <button
                    type="button"
                    onClick={() => setSnoozePickerOpen((open) => !open)}
                    className={viewStyles.actionButton}
                    disabled={snoozeBusy}
                    aria-haspopup="dialog"
                    aria-expanded={snoozePickerOpen}
                  >
                    {snoozeBusy ? "Snoozing…" : "Snooze"}
                  </button>
                  {snoozePickerOpen && (
                    <SnoozePicker
                      onPick={(until) => {
                        void handleSnooze(until);
                      }}
                      onCancel={() => setSnoozePickerOpen(false)}
                    />
                  )}
                </div>
              </div>
            </div>
            <dl className={viewStyles.headerList}>
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
          <ReadReceiptPrompt email={email} />
          <div className={viewStyles.body}>
            {htmlPart && email.bodyValues?.[htmlPart.partId!] ? (
              <HtmlMessageBody
                html={email.bodyValues[htmlPart.partId!].value}
                cidUrls={cidUrls}
              />
            ) : bodyText ? (
              <pre className={viewStyles.bodyPre}>{bodyText}</pre>
            ) : (
              <p className={viewStyles.muted}>(empty message body)</p>
            )}
          </div>
          <AttachmentPanel attachments={attachments} />
        </article>
      )}

      {/* WS7: smart reply chips */}
      {email && smartReplies.length > 0 && (
        <div className={viewStyles.smartReplies}>
          <span className="mr-2 text-xs text-fg-muted">Quick reply:</span>
          {smartReplies.map((s, i) => (
            <button
              key={i}
              type="button"
              className={viewStyles.smartReplyChip}
              onClick={() =>
                navigate("/mail/compose", {
                  state: {
                    mode: "reply",
                    sourceEmailId: email.id,
                    to: replyTarget,
                    subject: withPrefix(email.subject, "Re:"),
                    prefillBody: s.text,
                    quotedBody: bodyText,
                    quotedFrom: email.from,
                    quotedDate: email.receivedAt,
                  },
                })
              }
            >
              {s.text}
            </button>
          ))}
        </div>
      )}

      {/* WS7: unsubscribe helper */}
      {email && unsubInfo && unsubInfo.unsubscribe && !unsubInfo.already_done && (
        <div className="my-2 rounded-md bg-warning-bg px-4 py-2 text-warning-fg">
          <span className="mr-2">This looks like a mailing list.</span>
          <button
            type="button"
            disabled={unsubBusy}
            className="cursor-pointer rounded-sm border border-border px-3 py-1"
            onClick={() => {
              setUnsubBusy(true);
              postUnsubscribe(email.id)
                .then(() => setUnsubInfo((prev) => prev ? { ...prev, already_done: true } : prev))
                .catch(() => { /* best-effort */ })
                .finally(() => setUnsubBusy(false));
            }}
          >
            {unsubBusy ? "Unsubscribing…" : "Unsubscribe"}
          </button>
        </div>
      )}
      {email && unsubInfo?.already_done && (
        <div className="my-2 rounded-md bg-success-bg px-4 py-2 text-success-fg">
          You have unsubscribed from this list.
        </div>
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

/**
 * Remove from `list` any address whose email (case-insensitively)
 * matches one in `exclude`. Used to prevent duplicate recipients
 * between the To and Cc fields on Reply-All.
 */
function dedupeAddresses(
  list: { name: string | null; email: string }[],
  exclude: { name: string | null; email: string }[],
): { name: string | null; email: string }[] {
  const seen = new Set<string>();
  for (const a of exclude) seen.add(a.email.trim().toLowerCase());
  const out: { name: string | null; email: string }[] = [];
  for (const a of list) {
    const key = a.email.trim().toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(a);
  }
  return out;
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


/**
 * Tailwind class recipes for the message view. Mapped onto the
 * semantic design tokens so the article, header grid and chips track
 * the active light/dark theme.
 */
const viewStyles: Record<string, string> = {
  root: "max-w-[900px] p-4",
  topBar: "mb-3",
  backLink: "text-sm text-primary no-underline hover:underline",
  article: "rounded-lg border border-border bg-surface p-5",
  header: "mb-4 border-b border-border pb-3",
  subjectRow: "mb-3 flex items-start justify-between gap-3",
  subject: "m-0 text-xl font-semibold",
  actions: "flex shrink-0 gap-1",
  actionButton:
    "cursor-pointer rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs text-fg transition-colors hover:bg-surface-hover",
  snoozeWrap: "relative inline-block",
  snoozeConfirmation:
    "mb-3 rounded-md border border-success/40 bg-success-bg px-3 py-2 text-sm text-success-fg",
  attachmentsBox: "mt-4 border-t border-border pt-3",
  attachmentsTitle: "mb-2 mt-0 text-sm font-semibold",
  attachmentsList: "m-0 flex list-none flex-col gap-1 p-0 text-sm",
  attachmentName: "mr-2",
  attachmentMeta: "text-fg-muted",
  headerList:
    "m-0 grid grid-cols-[80px_1fr] gap-x-3 gap-y-1 text-sm",
  body: "leading-relaxed",
  bodyPre: "m-0 whitespace-pre-wrap font-[inherit] text-sm",
  error: "rounded-md bg-danger-bg px-3 py-2 text-danger-fg",
  muted: "italic text-fg-muted",
  smartReplies: "flex flex-wrap items-center gap-2 py-3",
  smartReplyChip:
    "cursor-pointer whitespace-nowrap rounded-pill border border-primary/30 bg-primary-subtle px-3.5 py-1.5 text-sm text-primary transition-colors hover:bg-primary-subtle/70",
};
