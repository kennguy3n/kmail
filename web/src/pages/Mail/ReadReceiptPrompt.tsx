import { useMemo, useState } from "react";

import { jmapClient } from "../../api/jmap";
import { readJSON, writeJSON } from "../../api/localStore";
import type { Email, Identity } from "../../types";

/**
 * Read-receipt (MDN) prompt banner.
 *
 * When the opened message carries a `Disposition-Notification-To`
 * header (RFC 8098), the sender asked to be told the message was
 * read. We never send an MDN automatically — that leaks read status
 * without consent — so this banner asks the user to confirm. The
 * decision (sent or dismissed) is remembered per message id so the
 * prompt doesn't reappear on every open.
 */
const RESPONDED_KEY = "mdn.responded";

// Cap the remembered-id list so it can't grow without bound over a
// long-lived session; only the most recently responded-to ids are
// kept (older receipts won't be re-prompted in practice because the
// messages have long since scrolled out of view).
const MAX_RESPONDED = 500;

function loadResponded(): string[] {
  return readJSON<string[]>(RESPONDED_KEY, []);
}

function markResponded(emailId: string): void {
  const list = loadResponded();
  if (list.includes(emailId)) return;
  writeJSON(RESPONDED_KEY, [...list, emailId].slice(-MAX_RESPONDED));
}

/** Extract the bare address from an RFC 5322 `Name <addr>` header. */
function parseAddress(header: string): string {
  const angle = header.match(/<([^>]+)>/);
  if (angle) return angle[1].trim();
  return header.trim();
}

/**
 * Pick the identity to send the MDN from: the one whose address was
 * among the message's recipients (we are confirming *we* read it),
 * falling back to the first identity.
 */
function pickIdentity(identities: Identity[], email: Email): Identity | null {
  if (identities.length === 0) return null;
  const recipientEmails = new Set(
    [...(email.to ?? []), ...(email.cc ?? [])].map((a) =>
      a.email.trim().toLowerCase(),
    ),
  );
  return (
    identities.find((id) => recipientEmails.has(id.email.trim().toLowerCase())) ??
    identities[0]
  );
}

export default function ReadReceiptPrompt({ email }: { email: Email }) {
  const requestHeader = email["header:Disposition-Notification-To:asText"];
  const alreadyResponded = useMemo(
    () => loadResponded().includes(email.id),
    [email.id],
  );
  const [state, setState] = useState<"idle" | "sending" | "sent" | "error">(
    "idle",
  );
  const [dismissed, setDismissed] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (!requestHeader || requestHeader.trim() === "") return null;
  if (alreadyResponded || dismissed || state === "sent") return null;

  const onSend = async () => {
    setState("sending");
    setError(null);
    try {
      const identities = await jmapClient.getIdentities();
      const fromIdentity = pickIdentity(identities, email);
      if (!fromIdentity) {
        throw new Error("No sending identity available for a read receipt.");
      }
      await jmapClient.sendReadReceipt({
        to: parseAddress(requestHeader),
        fromIdentity,
        originalMessageId: email["header:Message-ID:asText"] ?? null,
        originalSubject: email.subject ?? null,
        originalRecipient: fromIdentity.email,
      });
      markResponded(email.id);
      setState("sent");
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
      setState("error");
    }
  };

  const onDismiss = () => {
    markResponded(email.id);
    setDismissed(true);
  };

  return (
    <div className={styles.banner} role="status">
      <span className={styles.text}>
        The sender requested a read receipt for this message.
      </span>
      {error && <span className={styles.error}>{error}</span>}
      <span className={styles.spacer} />
      <button
        type="button"
        onClick={() => void onSend()}
        disabled={state === "sending"}
        className={styles.sendButton}
      >
        {state === "sending" ? "Sending…" : "Send receipt"}
      </button>
      <button type="button" onClick={onDismiss} className={styles.dismissButton}>
        Not now
      </button>
    </div>
  );
}

/** Theme-aware Tailwind class recipes for the ReadReceiptPrompt banner. */
const styles: Record<string, string> = {
  banner:
    "mb-3 flex flex-wrap items-center gap-2 rounded-md border border-warning/40 bg-warning-bg px-3 py-2 text-sm text-warning-fg",
  text: "font-medium",
  spacer: "flex-1",
  error: "basis-full text-danger-fg",
  sendButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3 py-1.5 text-xs font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  dismissButton:
    "cursor-pointer rounded-md border border-border bg-surface px-3 py-1.5 text-xs text-fg transition-colors hover:bg-surface-hover",
};
