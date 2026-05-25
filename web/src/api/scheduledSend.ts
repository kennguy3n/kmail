// Scheduled Send (WS4) client surface.
//
// Backed by `internal/scheduledsend/handlers.go`. The hook path
// (set the `X-KMail-Schedule-At` header on a normal JMAP
// EmailSubmission/set) is invoked through `JMAPClient.sendEmail`;
// this module wraps the list / cancel / status REST endpoints
// the Scheduled Sends page consumes.

import { DEV_BEARER_TOKEN } from "./jmap";

/** Server-side status values for a scheduled send. */
export type ScheduledSendStatus =
  | "pending"
  | "sent"
  | "cancelled"
  | "failed";

/** Body shape returned by `GET /api/v1/scheduled-sends`. */
export interface ScheduledSendSnapshot {
  id: string;
  status: ScheduledSendStatus;
  email_id: string;
  identity_id: string;
  stalwart_account_id?: string;
  send_at: string;
  attempts: number;
  last_error?: string;
  sent_at?: string;
  created_at: string;
}

/** Response shape for list. */
interface ListResponse {
  scheduled_sends: ScheduledSendSnapshot[];
}

/**
 * List every scheduled send owned by the authenticated user.
 * Server orders by created_at DESC (newest first) and caps at
 * 500 rows; the page is the primary consumer and a single user
 * never has that many pending scheduled sends in practice.
 */
export async function listScheduledSends(): Promise<ScheduledSendSnapshot[]> {
  const res = await fetch("/api/v1/scheduled-sends", {
    method: "GET",
    credentials: "include",
    headers: {
      Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
      Accept: "application/json",
    },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`listScheduledSends: ${res.status} ${text}`);
  }
  const body = (await res.json()) as ListResponse;
  return body.scheduled_sends ?? [];
}

/** Fetch a single scheduled send by id. */
export async function getScheduledSend(
  id: string,
): Promise<ScheduledSendSnapshot> {
  const res = await fetch(
    `/api/v1/scheduled-sends/${encodeURIComponent(id)}`,
    {
      method: "GET",
      credentials: "include",
      headers: {
        Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
        Accept: "application/json",
      },
    },
  );
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`getScheduledSend: ${res.status} ${text}`);
  }
  return (await res.json()) as ScheduledSendSnapshot;
}

/**
 * Cancel a pending scheduled send. Resolves to
 * `{ cancelled: true }` whether the row was previously pending
 * (just-cancelled) or already cancelled (idempotent re-cancel),
 * and `{ cancelled: false }` when the worker has already
 * dispatched the message (HTTP 410 Gone).
 *
 * Throws on transport / unexpected status codes so the page can
 * surface a generic error toast.
 */
export async function cancelScheduledSend(
  id: string,
): Promise<{ cancelled: boolean }> {
  const res = await fetch(
    `/api/v1/scheduled-sends/${encodeURIComponent(id)}`,
    {
      method: "DELETE",
      credentials: "include",
      headers: {
        Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
        Accept: "application/json",
      },
    },
  );
  if (res.status === 200) {
    return { cancelled: true };
  }
  if (res.status === 410) {
    return { cancelled: false };
  }
  const text = await res.text().catch(() => res.statusText);
  throw new Error(`cancelScheduledSend: ${res.status} ${text}`);
}
