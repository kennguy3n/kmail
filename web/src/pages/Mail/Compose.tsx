import {
  FormEvent,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { cn } from "../../lib/cn";

import {
  ATTACHMENT_LINK_THRESHOLD_BYTES,
  jmapClient,
  uploadLargeAttachment,
  type AttachmentLinkResponse,
} from "../../api/jmap";
import {
  getFrequentContacts,
  getCoRecipients,
  recordSend,
  type FrequentContact,
  type CoRecipientSuggestion,
} from "../../api/smart";
import { createSecureMessage } from "../../api/confidentialSend";
import { cancelPendingSend } from "../../api/undoSend";
import { useTenantSelection } from "../Admin/useTenantSelection";
import {
  defaultSignatureFor,
  listSignatures,
} from "../../api/signatures";
import { listTemplates } from "../../api/templates";
import RichTextEditor from "./RichTextEditor";
import ContactPicker from "./ContactPicker";
import TemplatePicker from "./TemplatePicker";
import { formatBytes } from "./messageContent";
import {
  escapeHtml,
  htmlToPlainText,
  isHtmlEmpty,
  plainTextToHtml,
} from "./richText";
import { newId as genId } from "../../api/localStore";
import type {
  DraftAttachment,
  EmailAddress,
  EmailDraft,
  Identity,
  Mailbox,
  PrivacyMode,
  Signature,
} from "../../types";

/** Compose-side options that only apply when privacyMode is "confidential-send". */
type ConfidentialOptions = {
  expirySeconds: number;
  passwordEnabled: boolean;
  password: string;
  /** -1 represents "unlimited"; the BFF clamps via `max_views`. */
  maxViews: number;
};

const DEFAULT_CONFIDENTIAL: ConfidentialOptions = {
  expirySeconds: 24 * 60 * 60,
  passwordEnabled: false,
  password: "",
  maxViews: 1,
};

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
  const [confidential, setConfidential] = useState<ConfidentialOptions>(
    DEFAULT_CONFIDENTIAL,
  );
  const [secureLink, setSecureLink] = useState<string | null>(null);
  const [linkCopied, setLinkCopied] = useState(false);
  // Confidential-send portal needs a tenant id; reuse the same
  // hook the admin pages use so the selection survives reloads.
  const { selectedTenantId } = useTenantSelection();
  const [selectedIdentityId, setSelectedIdentityId] = useState<string>("");
  const [isSending, setSending] = useState(false);
  const [isSavingDraft, setSavingDraft] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);
  // The id of the draft we saved most recently in this compose
  // session. Used to replace rather than duplicate the draft on
  // subsequent Save clicks.
  const [savedDraftId, setSavedDraftId] = useState<string | null>(null);
  // Attachments > 10 MB are uploaded to zk-object-fabric out of
  // band and replaced in the body with a presigned download link
  // (docs/PROPOSAL.md §9 attachment-to-link). Smaller files still
  // go through the normal JMAP Upload path — the UI surface below
  // only exposes the link-conversion flow for large files.
  const [attachmentLinks, setAttachmentLinks] = useState<AttachmentLinkResponse[]>([]);
  const [attachmentUploading, setAttachmentUploading] = useState(false);
  const [attachmentError, setAttachmentError] = useState<string | null>(null);
  // Handle for the deferred post-send navigation. We hold it in a
  // ref so the unmount cleanup can cancel it — otherwise a user
  // who navigates away in the 600 ms success window gets yanked
  // back to /mail.
  const navTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Undo-Send banner state. Populated when the BFF proxy hook
  // reports `X-KMail-Pending-Send-Id` on the EmailSubmission/set
  // response. `remainingMs` ticks down at 250ms cadence so the
  // banner renders a smooth second-by-second countdown; at zero
  // the banner clears and Compose navigates the user to the
  // inbox (Stalwart has accepted the submission at that point).
  const [pendingSend, setPendingSend] =
    useState<{ id: string; deadline: Date; recipients: string[] } | null>(
      null,
    );
  const [remainingMs, setRemainingMs] = useState(0);
  const [isCancelling, setCancelling] = useState(false);
  // Guards the undo-send commit path so the held send's recipients are
  // recorded at most once when the hold window elapses (the deadline
  // timer can fire more than once before the cleared state propagates).
  const recordedPendingRef = useRef<string | null>(null);

  // WS7: frequent contacts + co-recipient suggestions
  const [frequentContacts, setFrequentContacts] = useState<FrequentContact[]>([]);
  const [coRecipients, setCoRecipients] = useState<CoRecipientSuggestion[]>([]);

  useEffect(() => {
    getFrequentContacts(8)
      .then((r) => setFrequentContacts(r.contacts ?? []))
      .catch(() => { /* best-effort */ });
  }, []);

  useEffect(() => {
    // Parse with the same RFC 5322-aware splitter used for sending, so a
    // quoted display name containing a comma (e.g. `"Smith, John" <j@x>`)
    // isn't shredded into a non-address fragment. We key on the bare
    // emails: the anchor + exclude list the backend expects.
    const existing = parseAddresses(to)
      .map((a) => a.email.trim())
      .filter(Boolean);
    const first = existing[0];
    if (!first || !first.includes("@")) {
      setCoRecipients([]);
      return;
    }
    // Debounce so a burst of keystrokes fires a single request, and
    // guard against out-of-order responses: a stale in-flight reply
    // (resolved after a newer `to` value) must not overwrite state.
    let cancelled = false;
    const timer = setTimeout(() => {
      getCoRecipients(first, existing)
        .then((r) => {
          if (!cancelled) setCoRecipients(r.suggestions ?? []);
        })
        .catch(() => {
          if (!cancelled) setCoRecipients([]);
        });
    }, 250);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [to]);

  // Scheduled Send (WS4) state. `sendMode` toggles the Send
  // button between immediate dispatch and the schedule picker.
  // The picker uses a single datetime-local input — converting to
  // a Date for the BFF header is handled at submit time. When
  // the BFF accepts the schedule it returns row id + resolved
  // send-at; we surface those in `scheduledConfirm` so the user
  // gets a "scheduled for X" toast plus a link to the page.
  const [sendMode, setSendMode] = useState<"now" | "schedule">("now");
  const [scheduleAtLocal, setScheduleAtLocal] = useState<string>(() =>
    defaultScheduleLocalISO(),
  );
  const [scheduledConfirm, setScheduledConfirm] =
    useState<{ id: string; sendAt: Date } | null>(null);

  // Rich-text composition (WS2). `body` holds the plain-text source
  // of truth; `html` holds the rich body. `bodyMode` selects which
  // editor is active; toggling converts between the two so no
  // content is lost.
  const [bodyMode, setBodyMode] = useState<"rich" | "plain">("rich");
  const [html, setHtml] = useState(() =>
    seed?.quotedBody ? plainTextToHtml(initialBody(seed)) : "",
  );
  // Real MIME attachments (files + inline images) referenced by JMAP
  // blob id. Inline images additionally carry a `cid` so the HTML
  // body can reference them via `cid:` after a send-time rewrite.
  const [attachments, setAttachments] = useState<DraftAttachment[]>([]);
  // Maps an in-editor object-URL to the `cid` we'll rewrite it to at
  // send time. Held in a ref so editor re-renders don't churn it.
  const inlineCidRef = useRef<Map<string, string>>(new Map());
  const [signatures, setSignatures] = useState<Signature[]>([]);
  const [signatureId, setSignatureId] = useState<string>("");
  const [templates, setTemplates] = useState(() => listTemplates());
  const [templatePickerOpen, setTemplatePickerOpen] = useState(false);
  const [requestReceipt, setRequestReceipt] = useState(false);
  const [isDragging, setDragging] = useState(false);
  const [fileUploading, setFileUploading] = useState(false);

  useEffect(() => {
    // Capture the ref's Map instance so the cleanup revokes whatever
    // object URLs exist at unmount even if the ref were reassigned.
    const inlineUrls = inlineCidRef.current;
    return () => {
      if (navTimerRef.current) {
        clearTimeout(navTimerRef.current);
        navTimerRef.current = null;
      }
      // Inline-image object URLs (keys of the cid map) live for the
      // whole compose session; revoke them on unmount so a long
      // session that pastes many images doesn't leak blob references.
      for (const objectUrl of inlineUrls.keys()) {
        URL.revokeObjectURL(objectUrl);
      }
      inlineUrls.clear();
    };
  }, []);

  useEffect(() => {
    if (!pendingSend) {
      setRemainingMs(0);
      return;
    }
    const tick = () => {
      const ms = pendingSend.deadline.getTime() - Date.now();
      setRemainingMs(Math.max(0, ms));
      if (ms <= 0) {
        // Deadline passed — Stalwart will have accepted the
        // submission, so the send is now irrevocable. This is the
        // commit point for an undo-send: only now do we feed the
        // recipients to the frequent-contacts / co-recipient tracker,
        // so a send the user cancelled during the hold never pollutes
        // the suggestion graph. Guarded by a ref to record once.
        if (recordedPendingRef.current !== pendingSend.id) {
          recordedPendingRef.current = pendingSend.id;
          recordRecipients(pendingSend.recipients);
        }
        setPendingSend(null);
        setSuccessMessage("Message sent.");
        setSending(false);
        navTimerRef.current = setTimeout(() => {
          navTimerRef.current = null;
          navigate("/mail");
        }, 600);
      }
    };
    tick();
    const t = setInterval(tick, 250);
    return () => clearInterval(t);
  }, [pendingSend, navigate]);

  const handleCancelUndoSend = async () => {
    if (!pendingSend) return;
    setCancelling(true);
    try {
      const result = await cancelPendingSend(pendingSend.id);
      setPendingSend(null);
      if (result.cancelled) {
        setSuccessMessage("Send cancelled. Edit the message and send again.");
      } else {
        // Worker won the race; the message is already on its way.
        setSuccessMessage(
          "Too late — the message was dispatched before we could cancel.",
        );
        navTimerRef.current = setTimeout(() => {
          navTimerRef.current = null;
          navigate("/mail");
        }, 1200);
      }
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setCancelling(false);
      setSending(false);
    }
  };

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

  useEffect(() => {
    setSignatures(listSignatures());
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

  // Preselect the identity's default signature once both the
  // identity and the signature list are known. We only seed an
  // initial choice — a user who picks "No signature" or another one
  // is never overridden.
  const signatureSeededRef = useRef(false);
  useEffect(() => {
    if (signatureSeededRef.current) return;
    if (!identity || signatures.length === 0) return;
    const def = defaultSignatureFor(identity.email);
    if (def) {
      setSignatureId(def.id);
      signatureSeededRef.current = true;
    }
  }, [identity, signatures]);

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
    // Strip the sender's own identity from every recipient bucket
    // so Reply-All (and plain typed-in self-addresses) don't end up
    // mailing the sender a copy.
    const self = identity.email.trim().toLowerCase();
    const strip = (list: EmailAddress[]): EmailAddress[] =>
      list.filter((a) => a.email.trim().toLowerCase() !== self);
    const toList = strip(parseAddresses(to));
    if (requireTo && toList.length === 0) return null;

    const signature = signatures.find((s) => s.id === signatureId) ?? null;
    const draft: EmailDraft = {
      mailboxIds: { [draftsMailbox.id]: true },
      from: [{ name: identity.name || null, email: identity.email }],
      to: toList,
      cc: strip(parseAddresses(cc)),
      bcc: strip(parseAddresses(bcc)),
      subject: subject.trim(),
      privacyMode,
      identityId: identity.id,
    };

    if (bodyMode === "rich") {
      // Rewrite in-editor object URLs for pasted/inserted images to
      // the `cid:` references their inline MIME parts will carry, so
      // the recipient's client resolves them against the attachment.
      let outHtml = rewriteInlineCids(html, inlineCidRef.current);
      if (signature) outHtml = appendHtmlSignature(outHtml, signature.html);
      draft.htmlBody = outHtml;
      // Always carry a text/plain alternative for non-HTML clients.
      draft.textBody = htmlToPlainText(outHtml);
    } else {
      let outText = body;
      if (signature) {
        outText = `${outText}\n\n-- \n${htmlToPlainText(signature.html)}`;
      }
      draft.textBody = outText;
    }

    if (attachments.length > 0) draft.attachments = attachments;
    if (requestReceipt) draft.readReceiptTo = identity.email;
    return draft;
  };

  const handleSend = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setSuccessMessage(null);
    setScheduledConfirm(null);
    const draft = buildDraft(true);
    if (!draft) {
      setError("Please supply at least one recipient and a sender identity.");
      return;
    }
    let scheduleAtDate: Date | null = null;
    if (sendMode === "schedule") {
      const d = parseLocalDatetime(scheduleAtLocal);
      if (!d) {
        setError("Please pick a valid date and time to schedule the send.");
        return;
      }
      // Mirror the BFF MinScheduleHorizon (1 minute). Clicking
      // "Send" with a past or imminent time would otherwise hit
      // a 400 from the proxy hook; catching it client-side gives
      // a friendlier message.
      if (d.getTime() - Date.now() < 60_000) {
        setError("Scheduled time must be at least 1 minute in the future.");
        return;
      }
      scheduleAtDate = d;
      if (privacyMode === "confidential-send") {
        setError(
          "Confidential Send can't be combined with Scheduled Send yet — uncheck the schedule or change the privacy mode.",
        );
        return;
      }
    }
    setSending(true);
    try {
      // Pass the previously saved draft id so the client can
      // destroy that stale draft in the same Email/set call as the
      // submission; otherwise Save-then-Send would leave an
      // orphaned draft in the Drafts mailbox.
      //
      // Undo-Send (Phase 9 / WS3): opt in for non-confidential
      // sends only. Confidential Send mints a portal link
      // *after* the submission, so holding the submission would
      // require deferring the portal link too — keep the simpler
      // existing flow for now and revisit when Compose adds a
      // schedule-time picker.
      const sendResult = await jmapClient.sendEmail(draft, savedDraftId, {
        undoSend: privacyMode !== "confidential-send" && !scheduleAtDate,
        scheduleAt: scheduleAtDate ?? undefined,
      });
      setSavedDraftId(null);

      // WS7: the visible recipients we may feed to the frequent-contacts
      // / co-recipient tracker. Computed once; *when* we record depends
      // on the send mode (see recordRecipients calls below) so a send
      // the user can still cancel never pollutes the suggestion graph.
      const sentRecipients = recipientsToRecord(draft);

      if (sendResult.scheduledSendId && sendResult.scheduledSendAt) {
        // BFF persisted the submission to Postgres; the worker
        // will dispatch it at send_at. Render a confirmation
        // toast that links the user to the Scheduled Sends page
        // where they can cancel/inspect the row.
        // A scheduled send is committed to Postgres and will be
        // dispatched by the worker; record now (parity with the
        // reviewer-confirmed immediate/confidential commit points).
        recordRecipients(sentRecipients);
        setScheduledConfirm({
          id: sendResult.scheduledSendId,
          sendAt: sendResult.scheduledSendAt,
        });
        setSuccessMessage(null);
        // Don't auto-navigate — leave the compose page so the
        // user can confirm what was scheduled before moving on.
        // The toast itself carries a link to /mail/scheduled.
        setSending(false);
        return;
      }

      if (privacyMode === "confidential-send") {
        // Confidential Send never opts into undo-send, so the JMAP
        // submission already committed above — record now.
        recordRecipients(sentRecipients);
        // For Confidential Send we *additionally* mint a one-time
        // portal link. The encrypted blob ref is the JMAP message
        // id — the actual ciphertext envelope still lives in
        // zk-object-fabric (see do-not-do: do not reimplement
        // object storage / encryption envelopes here).
        if (!selectedTenantId) {
          setSuccessMessage(
            "Message sent, but Confidential Send requires a tenant selection (see Admin).",
          );
          setSending(false);
          return;
        }
        const link = await createSecureMessage({
          tenantId: selectedTenantId,
          senderId: identity?.email ?? "unknown",
          encryptedBlobRef: sendResult.emailId,
          password: confidential.passwordEnabled ? confidential.password : undefined,
          expiresInSeconds: confidential.expirySeconds,
          maxViews: confidential.maxViews <= 0 ? 0 : confidential.maxViews,
        });
        setSecureLink(`${window.location.origin}/secure/${link.link_token}`);
        setSuccessMessage("Confidential message sent. Share the secure link with the recipient.");
        setSending(false);
        return;
      }

      if (sendResult.pendingSendId && sendResult.undoDeadline) {
        // BFF intercepted the submission and is holding it in
        // Valkey. Render the undo banner; the deadline-timer
        // effect handles navigation after the hold elapses.
        // Recipients are deliberately NOT recorded here — the send is
        // still cancellable. We stash them on the pending state and
        // record only once the hold elapses (see the timer effect).
        recordedPendingRef.current = null;
        setPendingSend({
          id: sendResult.pendingSendId,
          deadline: sendResult.undoDeadline,
          recipients: sentRecipients,
        });
        setSuccessMessage(null);
        return;
      }

      // Immediate send: irrevocable once sendEmail resolved.
      recordRecipients(sentRecipients);
      setSuccessMessage("Message sent.");
      // Give the user a brief moment to see the success confirmation
      // before we navigate them back to the inbox. We deliberately
      // leave `isSending` true so the Send button stays disabled
      // through the navigation delay — resetting it here would let
      // a rapid second click dispatch a duplicate submission. The
      // timer id is tracked on a ref so the unmount cleanup can
      // cancel it if the user navigates away themselves.
      navTimerRef.current = setTimeout(() => {
        navTimerRef.current = null;
        navigate("/mail");
      }, 600);
    } catch (err: unknown) {
      setError(errorMessage(err));
      setSending(false);
    }
  };

  const onCopyLink = async () => {
    if (!secureLink) return;
    try {
      await navigator.clipboard.writeText(secureLink);
      setLinkCopied(true);
      setTimeout(() => setLinkCopied(false), 1500);
    } catch {
      // ignore clipboard errors — the link is still rendered.
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
      // Pass the previously-saved draft id so the client can batch
      // destroy+create in a single Email/set call — otherwise the
      // Drafts mailbox would accumulate one copy per Save click.
      const newId = await jmapClient.saveDraft(draft, savedDraftId);
      setSavedDraftId(newId);
      setSuccessMessage("Draft saved.");
    } catch (err: unknown) {
      setError(errorMessage(err));
    } finally {
      setSavingDraft(false);
    }
  };

  // Upload a pasted/inserted image as an inline attachment and hand
  // the editor an object URL to display now; the URL→cid mapping is
  // resolved into a real `cid:` reference at send time.
  const handleInlineImageUpload = async (file: File): Promise<string> => {
    const { blobId, type, size } = await jmapClient.uploadBlob(file, file.name);
    const cid = `${genId()}@kmail`;
    const objectUrl = URL.createObjectURL(file);
    inlineCidRef.current.set(objectUrl, cid);
    setAttachments((cur) => [
      ...cur,
      {
        blobId,
        name: file.name || "image",
        type: type || file.type || "image/png",
        size,
        inline: true,
        cid,
      },
    ]);
    return objectUrl;
  };

  // Upload one or more files as ordinary (non-inline) attachments.
  const handleFiles = async (files: FileList | File[]) => {
    const list = Array.from(files);
    if (list.length === 0) return;
    setFileUploading(true);
    setAttachmentError(null);
    try {
      for (const f of list) {
        const { blobId, type, size } = await jmapClient.uploadBlob(f, f.name);
        setAttachments((cur) => [
          ...cur,
          {
            blobId,
            name: f.name || "attachment",
            type: type || f.type || "application/octet-stream",
            size,
            inline: false,
          },
        ]);
      }
    } catch (err: unknown) {
      setAttachmentError(err instanceof Error ? err.message : String(err));
    } finally {
      setFileUploading(false);
    }
  };

  const removeAttachment = (index: number) => {
    setAttachments((cur) => cur.filter((_, i) => i !== index));
  };

  const toggleBodyMode = () => {
    if (bodyMode === "rich") {
      setBody(htmlToPlainText(html));
      setBodyMode("plain");
    } else {
      setHtml(plainTextToHtml(body));
      setBodyMode("rich");
    }
  };

  const applyTemplate = (result: { subject: string; body: string }) => {
    if (result.subject) setSubject(result.subject);
    if (bodyMode === "rich") {
      setHtml((cur) =>
        cur && !isHtmlEmpty(cur) ? `${cur}${result.body}` : result.body,
      );
    } else {
      const text = htmlToPlainText(result.body);
      setBody((cur) => (cur ? `${cur}\n${text}` : text));
    }
    setTemplatePickerOpen(false);
  };

  const onComposeDrop = (e: React.DragEvent<HTMLDivElement>) => {
    if (!e.dataTransfer.files || e.dataTransfer.files.length === 0) return;
    e.preventDefault();
    setDragging(false);
    void handleFiles(e.dataTransfer.files);
  };

  const heading =
    seed?.mode === "reply" || seed?.mode === "replyAll"
      ? "Reply"
      : seed?.mode === "forward"
        ? "Forward"
        : "New message";

  return (
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>{heading}</h2>
      </header>
      {error && (
        <div className={styles.error} role="alert">
          <span>{error}</span>
          <button
            type="button"
            onClick={() => setError(null)}
            className={styles.errorDismiss}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {successMessage && (
        <div className={styles.success} role="status">
          {successMessage}
        </div>
      )}
      {pendingSend && (
        <div className={styles.undoBanner} role="status" aria-live="polite">
          <span>
            Sending in {Math.ceil(remainingMs / 1000)}s…
          </span>
          <button
            type="button"
            onClick={handleCancelUndoSend}
            disabled={isCancelling}
            className={styles.undoCancel}
            data-testid="undo-send-cancel"
          >
            {isCancelling ? "Cancelling…" : "Undo"}
          </button>
        </div>
      )}
      {scheduledConfirm && (
        <div
          className={styles.scheduledBanner}
          role="status"
          aria-live="polite"
          data-testid="scheduled-send-confirm"
        >
          <span>
            Scheduled for{" "}
            {scheduledConfirm.sendAt.toLocaleString()}.{" "}
            <button
              type="button"
              onClick={() => navigate("/mail/scheduled")}
              className={styles.linkButton}
            >
              View scheduled sends
            </button>
          </span>
        </div>
      )}
      <form onSubmit={handleSend} className={styles.form}>
        <div className={styles.row}>
          <label htmlFor="compose-from" className={styles.label}>
            From
          </label>
          <select
            id="compose-from"
            value={selectedIdentityId}
            onChange={(e) => setSelectedIdentityId(e.target.value)}
            className={styles.select}
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
        <div className={styles.row}>
          <label htmlFor="compose-signature" className={styles.label}>
            Signature
          </label>
          <select
            id="compose-signature"
            value={signatureId}
            onChange={(e) => setSignatureId(e.target.value)}
            className={styles.select}
          >
            <option value="">No signature</option>
            {signatures.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
                {s.isDefault ? " (default)" : ""}
              </option>
            ))}
          </select>
        </div>
        <div className={styles.row}>
          <label htmlFor="compose-to" className={styles.label}>
            To
          </label>
          <ContactPicker
            id="compose-to"
            value={to}
            onChange={setTo}
            tenantId={selectedTenantId}
            placeholder="name@example.com, other@example.com"
            required
            ariaLabel="To recipients"
            inputClassName={styles.input}
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="compose-cc" className={styles.label}>
            Cc
          </label>
          <ContactPicker
            id="compose-cc"
            value={cc}
            onChange={setCc}
            tenantId={selectedTenantId}
            ariaLabel="Cc recipients"
            inputClassName={styles.input}
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="compose-bcc" className={styles.label}>
            Bcc
          </label>
          <ContactPicker
            id="compose-bcc"
            value={bcc}
            onChange={setBcc}
            tenantId={selectedTenantId}
            ariaLabel="Bcc recipients"
            inputClassName={styles.input}
          />
        </div>
        {/* WS7: frequent contacts + co-recipient suggestions */}
        {(frequentContacts.length > 0 || coRecipients.length > 0) && (
          <div className={cn(styles.row, "flex-wrap gap-1")}>
            <span className={cn(styles.label, "mt-1 self-start")}>
              Suggestions
            </span>
            <div className="flex flex-1 flex-wrap gap-1">
              {frequentContacts.map((c) => (
                <button
                  key={c.email}
                  type="button"
                  className="cursor-pointer whitespace-nowrap rounded-pill border border-primary/30 bg-primary-subtle px-2.5 py-0.5 text-xs text-primary"
                  onClick={() => {
                    const cur = to.trim();
                    setTo(cur ? `${cur}, ${c.email}` : c.email);
                  }}
                >
                  {c.name ? `${c.name} <${c.email}>` : c.email}
                </button>
              ))}
              {coRecipients.map((c) => (
                <button
                  key={c.email}
                  type="button"
                  className="cursor-pointer whitespace-nowrap rounded-pill border border-success/30 bg-success-bg px-2.5 py-0.5 text-xs text-success-fg"
                  onClick={() => {
                    const cur = cc.trim();
                    setCc(cur ? `${cur}, ${c.email}` : c.email);
                  }}
                >
                  + CC {c.name ? `${c.name} <${c.email}>` : c.email}
                </button>
              ))}
            </div>
          </div>
        )}
        <div className={styles.row}>
          <label htmlFor="compose-subject" className={styles.label}>
            Subject
          </label>
          <input
            id="compose-subject"
            type="text"
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            className={styles.input}
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="compose-privacy" className={styles.label}>
            Privacy
          </label>
          <select
            id="compose-privacy"
            value={privacyMode}
            onChange={(e) => setPrivacyMode(e.target.value as PrivacyMode)}
            className={styles.select}
          >
            <option value="standard">Standard Private Mail</option>
            <option value="confidential-send">Confidential Send</option>
            <option value="zero-access-vault">Zero-Access Vault</option>
          </select>
        </div>
        {privacyMode === "confidential-send" && (
          <div className={styles.row}>
            <label className={styles.label}>Secure portal</label>
            <div className="grid gap-2">
              <label>
                Expires in&nbsp;
                <select
                  value={confidential.expirySeconds}
                  onChange={(e) =>
                    setConfidential((c) => ({
                      ...c,
                      expirySeconds: Number(e.target.value),
                    }))
                  }
                >
                  <option value={60 * 60}>1 hour</option>
                  <option value={24 * 60 * 60}>24 hours</option>
                  <option value={7 * 24 * 60 * 60}>7 days</option>
                  <option value={30 * 24 * 60 * 60}>30 days</option>
                </select>
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={confidential.passwordEnabled}
                  onChange={(e) =>
                    setConfidential((c) => ({
                      ...c,
                      passwordEnabled: e.target.checked,
                    }))
                  }
                />
                &nbsp;Require password
              </label>
              {confidential.passwordEnabled && (
                <input
                  type="password"
                  placeholder="Recipient password"
                  value={confidential.password}
                  onChange={(e) =>
                    setConfidential((c) => ({ ...c, password: e.target.value }))
                  }
                />
              )}
              <label>
                Max views&nbsp;
                <select
                  value={confidential.maxViews}
                  onChange={(e) =>
                    setConfidential((c) => ({
                      ...c,
                      maxViews: Number(e.target.value),
                    }))
                  }
                >
                  <option value={1}>1</option>
                  <option value={3}>3</option>
                  <option value={-1}>Unlimited</option>
                </select>
              </label>
            </div>
          </div>
        )}
        {secureLink && (
          <div className={styles.row}>
            <label className={styles.label}>Secure link</label>
            <div className="grid gap-1">
              <code className="break-all">{secureLink}</code>
              <div>
                <button type="button" onClick={onCopyLink}>
                  {linkCopied ? "Copied!" : "Copy link"}
                </button>
              </div>
              <p className="m-0 text-sm text-fg-muted">
                Share this link with the recipient. The portal enforces
                expiry, password, and max-views automatically.
              </p>
            </div>
          </div>
        )}
        <div className={styles.row}>
          <label className={styles.label}>Attachments</label>
          <div>
            <input
              type="file"
              multiple
              aria-label="Attach files"
              onChange={(e) => {
                if (e.target.files && e.target.files.length > 0) {
                  void handleFiles(e.target.files);
                }
                e.target.value = "";
              }}
              disabled={fileUploading}
            />
            {fileUploading && <span>&nbsp;Uploading…</span>}
            {attachments.filter((a) => !a.inline).length > 0 && (
              <ul className={styles.attachmentList}>
                {attachments.map((a, i) =>
                  a.inline ? null : (
                    <li key={a.blobId || `att-${i}`} className={styles.attachmentItem}>
                      <span>
                        {a.name}
                        {typeof a.size === "number"
                          ? ` (${formatBytes(a.size)})`
                          : ""}
                      </span>
                      <button
                        type="button"
                        onClick={() => removeAttachment(i)}
                        className={styles.attachmentRemove}
                        aria-label={`Remove ${a.name}`}
                      >
                        ×
                      </button>
                    </li>
                  ),
                )}
              </ul>
            )}
            <details className={styles.largeFileDetails}>
              <summary className={styles.largeFileSummary}>
                Send a large file as a link (over 10 MB)
              </summary>
              <input
                type="file"
                aria-label="Large file to link"
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  if (!file) return;
                  if (file.size < ATTACHMENT_LINK_THRESHOLD_BYTES) {
                    setAttachmentError(
                      "This option is for files over 10 MB; smaller files can be attached directly above.",
                    );
                    e.target.value = "";
                    return;
                  }
                  setAttachmentError(null);
                  setAttachmentUploading(true);
                  uploadLargeAttachment(file)
                    .then((link) => {
                      setAttachmentLinks((cur) => [...cur, link]);
                      // Insert the link into whichever body the active
                      // mode actually sends: buildDraft reads `html` in
                      // rich mode and `body` in plain mode, so appending
                      // only to `body` would silently drop the link from
                      // a rich-text message.
                      if (bodyMode === "rich") {
                        const anchor = `<a href="${escapeHtml(link.url)}">${escapeHtml(link.filename)}</a>`;
                        setHtml(
                          (h) => `${h}<p>Attachment: ${anchor}</p>`,
                        );
                      } else {
                        setBody(
                          (b) =>
                            `${b}${b && !b.endsWith("\n") ? "\n" : ""}\nAttachment: ${link.filename} — ${link.url}\n`,
                        );
                      }
                    })
                    .catch((err: unknown) =>
                      setAttachmentError(
                        err instanceof Error ? err.message : String(err),
                      ),
                    )
                    .finally(() => {
                      setAttachmentUploading(false);
                      e.target.value = "";
                    });
                }}
                disabled={attachmentUploading}
              />
              {attachmentUploading && <span>&nbsp;Uploading…</span>}
              {attachmentLinks.length > 0 && (
                <ul className="mt-1 pl-[1.2rem]">
                  {attachmentLinks.map((a) => (
                    <li key={a.id || a.url}>
                      {a.filename} ({Math.round(a.size_bytes / 1024 / 1024)} MB)
                    </li>
                  ))}
                </ul>
              )}
            </details>
            {attachmentError && (
              <p role="alert" className="mt-1 text-danger-fg">
                {attachmentError}
              </p>
            )}
          </div>
        </div>
        <div className={styles.composeToolbar}>
          <button
            type="button"
            onClick={toggleBodyMode}
            className={styles.toolbarButton}
          >
            {bodyMode === "rich" ? "Switch to plain text" : "Switch to rich text"}
          </button>
          <button
            type="button"
            onClick={() => {
              setTemplates(listTemplates());
              setTemplatePickerOpen(true);
            }}
            className={styles.toolbarButton}
          >
            Insert template
          </button>
          <label className={styles.receiptLabel}>
            <input
              type="checkbox"
              checked={requestReceipt}
              onChange={(e) => setRequestReceipt(e.target.checked)}
            />
            &nbsp;Request read receipt
          </label>
        </div>
        <div
          className={cn(styles.bodyRow, isDragging && styles.bodyRowDragging)}
          onDragOver={(e) => {
            if (e.dataTransfer.types.includes("Files")) {
              e.preventDefault();
              setDragging(true);
            }
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onComposeDrop}
        >
          {bodyMode === "rich" ? (
            <RichTextEditor
              value={html}
              onChange={setHtml}
              placeholder="Write your message…"
              onImageUpload={handleInlineImageUpload}
              ariaLabel="Message body"
              minHeight={300}
            />
          ) : (
            <textarea
              aria-label="Message body"
              value={body}
              onChange={(e) => setBody(e.target.value)}
              placeholder="Write your message…"
              className={styles.textarea}
              rows={16}
            />
          )}
          {isDragging && (
            <div className={styles.dropHint}>Drop files to attach</div>
          )}
        </div>
        <div className={styles.buttonRow}>
          <button
            type="submit"
            disabled={!canSubmit}
            className={cn(
              styles.primaryButton,
              !canSubmit && "cursor-not-allowed opacity-60",
            )}
            data-testid="compose-send"
          >
            {isSending
              ? "Sending…"
              : sendMode === "schedule"
                ? "Schedule send"
                : "Send"}
          </button>
          <select
            aria-label="Send timing"
            value={sendMode}
            onChange={(e) => setSendMode(e.target.value as "now" | "schedule")}
            disabled={isSending}
            className={styles.secondarySelect}
            data-testid="compose-send-mode"
          >
            <option value="now">Send now</option>
            <option value="schedule">Schedule for later</option>
          </select>
          {sendMode === "schedule" && (
            <input
              type="datetime-local"
              aria-label="Schedule send for"
              value={scheduleAtLocal}
              onChange={(e) => setScheduleAtLocal(e.target.value)}
              disabled={isSending}
              className={styles.scheduleInput}
              data-testid="compose-schedule-at"
            />
          )}
          <button
            type="button"
            onClick={handleSaveDraft}
            disabled={isSending || isSavingDraft || !draftsMailbox}
            className={styles.secondaryButton}
          >
            {isSavingDraft ? "Saving…" : "Save draft"}
          </button>
          <button
            type="button"
            onClick={() => navigate(-1)}
            className={styles.secondaryButton}
            disabled={isSending}
          >
            Cancel
          </button>
        </div>
      </form>
      {templatePickerOpen && (
        <TemplatePicker
          templates={templates}
          senderName={identity?.name ?? ""}
          onApply={applyTemplate}
          onClose={() => setTemplatePickerOpen(false)}
        />
      )}
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
  /**
   * Non-quoted text to pre-fill at the top of the body (e.g. a
   * smart-reply suggestion). Rendered above any quoted reply so the
   * user starts with the suggested sentence and can edit from there.
   */
  prefillBody?: string;
}

function initialBody(seed: ComposeSeed | null): string {
  const prefill = seed?.prefillBody ?? "";
  if (!seed || !seed.quotedBody) return prefill;
  const header = buildQuoteHeader(seed);
  const quoted = seed.quotedBody
    .split("\n")
    .map((line) => `> ${line}`)
    .join("\n");
  return `${prefill}\n\n${header}\n${quoted}\n`;
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
 * Replace each in-editor object URL with the `cid:` reference of
 * the inline MIME part it maps to, so the sent HTML body resolves
 * inline images against their attachments (RFC 2392).
 */
function rewriteInlineCids(html: string, map: Map<string, string>): string {
  let out = html;
  for (const [url, cid] of map) {
    out = out.split(url).join(`cid:${cid}`);
  }
  return out;
}

/** Append a signature, separated by the standard `-- ` delimiter. */
function appendHtmlSignature(html: string, signatureHtml: string): string {
  return `${html}<br><div class="kmail-signature">-- <br>${signatureHtml}</div>`;
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
 * WS7: the recipients we feed to the frequent-contacts / co-recipient
 * tracker after a send commits.
 *
 * Only the *visible* recipients (To + Cc) are recorded. Bcc is
 * deliberately excluded: it builds the co-recipient graph, so recording
 * a blind-copied address could later surface it as a "you usually CC X"
 * suggestion and leak the hidden Bcc relationship to anyone composing on
 * this account. The full `Name <email>` form is sent (not the bare
 * email) so the tracker can capture the display name for the chips; the
 * BFF parses the address and keys on the bare email.
 */
function recipientsToRecord(draft: EmailDraft): string[] {
  return [...(draft.to ?? []), ...(draft.cc ?? [])].map((a) => formatAddress(a));
}

/**
 * Fire-and-forget the send record. Best-effort so a Valkey hiccup can
 * never fail or delay the send the user just confirmed; suggestions are
 * non-critical. Callers invoke this only once a send is irrevocable
 * (immediate/scheduled/confidential at submit, undo-send once the hold
 * elapses) so a cancelled send never pollutes the suggestion graph.
 */
function recordRecipients(recipients: string[]): void {
  if (recipients.length === 0) return;
  recordSend(recipients).catch(() => {
    /* suggestions are non-critical; ignore */
  });
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

/**
 * Compute the default value for the schedule-send
 * `<input type="datetime-local">`. We pick "one hour from now,
 * rounded down to the nearest minute" so the picker opens at a
 * sensible time without forcing the user to clear today's stale
 * value. The local-ISO format (`YYYY-MM-DDTHH:mm`, no timezone)
 * is what `datetime-local` expects.
 */
function defaultScheduleLocalISO(): string {
  const d = new Date(Date.now() + 60 * 60 * 1000);
  d.setSeconds(0, 0);
  return formatLocalISO(d);
}

function formatLocalISO(d: Date): string {
  const pad = (n: number): string => n.toString().padStart(2, "0");
  const year = d.getFullYear();
  const month = pad(d.getMonth() + 1);
  const day = pad(d.getDate());
  const hour = pad(d.getHours());
  const minute = pad(d.getMinutes());
  return `${year}-${month}-${day}T${hour}:${minute}`;
}

/**
 * Parse the value emitted by `<input type="datetime-local">`
 * (YYYY-MM-DDTHH:mm) into a Date interpreted in the local
 * timezone. Returns `null` if the string is malformed or the
 * resulting Date is invalid — the caller surfaces a friendly
 * error rather than firing a bad request at the BFF.
 */
function parseLocalDatetime(raw: string): Date | null {
  if (!raw) return null;
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return null;
  return d;
}


/**
 * Tailwind class recipes for the Compose form. Values map onto the
 * semantic design tokens (see `tailwind.config.ts`) so the form,
 * banners and controls follow the active light/dark theme instead of
 * the previous hard-coded palette.
 */
const styles: Record<string, string> = {
  root: "max-w-[900px] p-4",
  header: "mb-3",
  title: "m-0 text-xl font-semibold",
  form: "flex flex-col gap-2 rounded-lg border border-border bg-surface p-4",
  row: "grid grid-cols-[80px_1fr] items-center gap-2",
  bodyRow: "relative mt-1 flex flex-col",
  bodyRowDragging: "rounded-sm outline-dashed outline-2 outline-offset-2 outline-primary",
  dropHint:
    "pointer-events-none absolute inset-0 flex items-center justify-center rounded-sm bg-primary/10 font-semibold text-primary",
  composeToolbar: "mt-2 flex flex-wrap items-center gap-2",
  toolbarButton:
    "cursor-pointer rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs text-fg transition-colors hover:bg-surface-hover",
  receiptLabel: "ml-auto inline-flex items-center text-xs text-fg-muted",
  attachmentList: "m-0 mt-1.5 grid list-none gap-1 p-0",
  attachmentItem:
    "flex items-center justify-between gap-2 rounded-md bg-surface-muted px-2 py-1 text-xs text-fg",
  attachmentRemove:
    "cursor-pointer border-0 bg-transparent text-base leading-none text-fg-muted hover:text-fg",
  largeFileDetails: "mt-2 text-xs",
  largeFileSummary: "cursor-pointer text-fg-muted",
  label: "text-sm font-semibold text-fg-muted",
  input:
    "rounded-md border border-border bg-surface px-2 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  select:
    "rounded-md border border-border bg-surface px-2 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  textarea:
    "min-h-64 resize-y rounded-md border border-border bg-surface p-2.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  buttonRow: "mt-2 flex gap-2",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-4 py-2 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  secondaryButton:
    "cursor-pointer rounded-md border border-border bg-surface px-4 py-2 text-sm text-fg transition-colors hover:bg-surface-hover",
  error:
    "mb-2 flex items-center justify-between gap-2 rounded-md bg-danger-bg px-3 py-2 text-danger-fg",
  errorDismiss:
    "cursor-pointer border-0 bg-transparent px-1 text-lg leading-none text-danger-fg",
  success: "mb-2 rounded-md bg-success-bg px-3 py-2 text-success-fg",
  undoBanner:
    "mb-2 flex items-center justify-between gap-3 rounded-md bg-fg px-3 py-2 text-bg",
  undoCancel:
    "cursor-pointer rounded-md border border-bg bg-transparent px-3 py-1 font-semibold text-bg",
  scheduledBanner:
    "mb-2 flex items-center gap-3 rounded-md bg-info-bg px-3 py-2 text-info-fg",
  linkButton:
    "cursor-pointer border-0 bg-transparent p-0 font-[inherit] text-primary underline",
  secondarySelect:
    "rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg",
  scheduleInput:
    "rounded-md border border-border bg-surface px-3 py-2 text-sm text-fg",
};
