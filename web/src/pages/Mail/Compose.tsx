import {
  FormEvent,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation, useNavigate } from "react-router-dom";

import {
  ATTACHMENT_LINK_THRESHOLD_BYTES,
  jmapClient,
  uploadLargeAttachment,
  type AttachmentLinkResponse,
} from "../../api/jmap";
import {
  getFrequentContacts,
  getCoRecipients,
  type FrequentContact,
  type CoRecipientSuggestion,
} from "../../api/smart";
import { createSecureMessage } from "../../api/confidentialSend";
import { cancelPendingSend } from "../../api/undoSend";
import { useTenantSelection } from "../Admin/useTenantSelection";
import type {
  EmailAddress,
  EmailDraft,
  Identity,
  Mailbox,
  PrivacyMode,
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
    useState<{ id: string; deadline: Date } | null>(null);
  const [remainingMs, setRemainingMs] = useState(0);
  const [isCancelling, setCancelling] = useState(false);

  // WS7: frequent contacts + co-recipient suggestions
  const [frequentContacts, setFrequentContacts] = useState<FrequentContact[]>([]);
  const [coRecipients, setCoRecipients] = useState<CoRecipientSuggestion[]>([]);

  useEffect(() => {
    getFrequentContacts(8)
      .then((r) => setFrequentContacts(r.contacts ?? []))
      .catch(() => { /* best-effort */ });
  }, []);

  useEffect(() => {
    const existing = to.split(",").map((s) => s.trim()).filter(Boolean);
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

  useEffect(() => {
    return () => {
      if (navTimerRef.current) {
        clearTimeout(navTimerRef.current);
        navTimerRef.current = null;
      }
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
        // submission. Clear state and route to /mail.
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
    // Strip the sender's own identity from every recipient bucket
    // so Reply-All (and plain typed-in self-addresses) don't end up
    // mailing the sender a copy.
    const self = identity.email.trim().toLowerCase();
    const strip = (list: EmailAddress[]): EmailAddress[] =>
      list.filter((a) => a.email.trim().toLowerCase() !== self);
    const toList = strip(parseAddresses(to));
    if (requireTo && toList.length === 0) return null;
    return {
      mailboxIds: { [draftsMailbox.id]: true },
      from: [{ name: identity.name || null, email: identity.email }],
      to: toList,
      cc: strip(parseAddresses(cc)),
      bcc: strip(parseAddresses(bcc)),
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

      if (sendResult.scheduledSendId && sendResult.scheduledSendAt) {
        // BFF persisted the submission to Postgres; the worker
        // will dispatch it at send_at. Render a confirmation
        // toast that links the user to the Scheduled Sends page
        // where they can cancel/inspect the row.
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
        setPendingSend({
          id: sendResult.pendingSendId,
          deadline: sendResult.undoDeadline,
        });
        setSuccessMessage(null);
        return;
      }

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
      {pendingSend && (
        <div style={styles.undoBanner} role="status" aria-live="polite">
          <span>
            Sending in {Math.ceil(remainingMs / 1000)}s…
          </span>
          <button
            type="button"
            onClick={handleCancelUndoSend}
            disabled={isCancelling}
            style={styles.undoCancel}
            data-testid="undo-send-cancel"
          >
            {isCancelling ? "Cancelling…" : "Undo"}
          </button>
        </div>
      )}
      {scheduledConfirm && (
        <div
          style={styles.scheduledBanner}
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
              style={styles.linkButton}
            >
              View scheduled sends
            </button>
          </span>
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
        {/* WS7: frequent contacts + co-recipient suggestions */}
        {(frequentContacts.length > 0 || coRecipients.length > 0) && (
          <div style={{ ...styles.row, flexWrap: "wrap", gap: 4 }}>
            <span style={{ ...styles.label, alignSelf: "flex-start", marginTop: 4 }}>
              Suggestions
            </span>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 4, flex: 1 }}>
              {frequentContacts.map((c) => (
                <button
                  key={c.email}
                  type="button"
                  style={{
                    padding: "2px 10px",
                    fontSize: "0.8rem",
                    border: "1px solid #c2d9fd",
                    borderRadius: 12,
                    background: "#e8f0fe",
                    color: "#1a73e8",
                    cursor: "pointer",
                    whiteSpace: "nowrap",
                  }}
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
                  style={{
                    padding: "2px 10px",
                    fontSize: "0.8rem",
                    border: "1px solid #d4edda",
                    borderRadius: 12,
                    background: "#e8f5e9",
                    color: "#2e7d32",
                    cursor: "pointer",
                    whiteSpace: "nowrap",
                  }}
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
        {privacyMode === "confidential-send" && (
          <div style={styles.row}>
            <label style={styles.label}>Secure portal</label>
            <div style={{ display: "grid", gap: "0.5rem" }}>
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
          <div style={styles.row}>
            <label style={styles.label}>Secure link</label>
            <div style={{ display: "grid", gap: "0.25rem" }}>
              <code style={{ wordBreak: "break-all" }}>{secureLink}</code>
              <div>
                <button type="button" onClick={onCopyLink}>
                  {linkCopied ? "Copied!" : "Copy link"}
                </button>
              </div>
              <p style={{ margin: 0, color: "#475569", fontSize: "0.85rem" }}>
                Share this link with the recipient. The portal enforces
                expiry, password, and max-views automatically.
              </p>
            </div>
          </div>
        )}
        <div style={styles.row}>
          <label style={styles.label}>Attachments</label>
          <div>
            <input
              type="file"
              onChange={(e) => {
                const file = e.target.files?.[0];
                if (!file) return;
                if (file.size < ATTACHMENT_LINK_THRESHOLD_BYTES) {
                  setAttachmentError(
                    "Files under 10 MB are not yet supported by this form; use a larger file or paste inline.",
                  );
                  e.target.value = "";
                  return;
                }
                setAttachmentError(null);
                setAttachmentUploading(true);
                uploadLargeAttachment(file)
                  .then((link) => {
                    setAttachmentLinks((cur) => [...cur, link]);
                    setBody(
                      (b) =>
                        `${b}${b && !b.endsWith("\n") ? "\n" : ""}\nAttachment: ${link.filename} — ${link.url}\n`,
                    );
                  })
                  .catch((err: unknown) =>
                    setAttachmentError(err instanceof Error ? err.message : String(err)),
                  )
                  .finally(() => {
                    setAttachmentUploading(false);
                    e.target.value = "";
                  });
              }}
              disabled={attachmentUploading}
            />
            {attachmentUploading && <span>&nbsp;Uploading…</span>}
            {attachmentError && (
              <p role="alert" style={{ color: "#991b1b", margin: "0.25rem 0 0" }}>
                {attachmentError}
              </p>
            )}
            {attachmentLinks.length > 0 && (
              <ul style={{ margin: "0.25rem 0 0", paddingLeft: "1.2rem" }}>
                {attachmentLinks.map((a) => (
                  <li key={a.id || a.url}>
                    {a.filename} ({Math.round(a.size_bytes / 1024 / 1024)} MB)
                  </li>
                ))}
              </ul>
            )}
          </div>
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
            style={styles.secondarySelect}
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
              style={styles.scheduleInput}
              data-testid="compose-schedule-at"
            />
          )}
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
  undoBanner: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: "0.75rem",
    padding: "0.5rem 0.75rem",
    background: "#1f2937",
    color: "#f9fafb",
    borderRadius: "0.25rem",
    marginBottom: "0.5rem",
  },
  undoCancel: {
    padding: "0.25rem 0.75rem",
    border: "1px solid #f9fafb",
    background: "transparent",
    color: "#f9fafb",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontWeight: 600,
  },
  scheduledBanner: {
    display: "flex",
    alignItems: "center",
    gap: "0.75rem",
    padding: "0.5rem 0.75rem",
    background: "#e0f2fe",
    color: "#0c4a6e",
    borderRadius: "0.25rem",
    marginBottom: "0.5rem",
  },
  linkButton: {
    background: "transparent",
    border: "none",
    padding: 0,
    color: "#0369a1",
    cursor: "pointer",
    textDecoration: "underline",
    font: "inherit",
  },
  secondarySelect: {
    padding: "0.5rem 0.75rem",
    fontSize: "0.9rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    color: "#374151",
  },
  scheduleInput: {
    padding: "0.5rem 0.75rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    color: "#111827",
  },
};
