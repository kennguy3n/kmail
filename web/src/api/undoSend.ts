// Undo Send (Phase 9 / WS3) client surface.
//
// Backed by `internal/undosend/handlers.go`. The proxy-hook path
// (set the `X-KMail-Undo-Send: true` header on a normal JMAP
// EmailSubmission/set) is invoked through `JMAPClient.sendEmail`;
// this module wraps the cancel / status REST endpoints that the
// React Compose page hits after the BFF returns a pending-send id.

import { DEV_BEARER_TOKEN } from "./jmap";

/** Server-side status values for a pending send. */
export type PendingSendStatus =
  | "pending"
  | "sent"
  | "cancelled"
  | "failed";

/** Body shape returned by `GET /api/v1/send/{id}`. */
export interface PendingSendSnapshot {
  id: string;
  status: PendingSendStatus;
  email_id: string;
  created_at: string;
  deadline_at: string;
  attempts: number;
}

/**
 * Cancel a pending send. Resolves to `{ cancelled: true }` when
 * the BFF was able to remove the row from Valkey before the
 * worker dispatched, or `{ cancelled: false }` when the
 * dispatch already fired (HTTP 410 Gone).
 *
 * Throws on transport / unexpected status codes so the Compose
 * page can surface a generic error toast.
 */
export async function cancelPendingSend(
  id: string,
): Promise<{ cancelled: boolean }> {
  const res = await fetch(
    `/api/v1/send/${encodeURIComponent(id)}/cancel`,
    {
      method: "POST",
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
  throw new Error(`cancelPendingSend: ${res.status} ${text}`);
}

/** Read the current status of a pending send. */
export async function getPendingSendStatus(
  id: string,
): Promise<PendingSendSnapshot> {
  const res = await fetch(`/api/v1/send/${encodeURIComponent(id)}`, {
    method: "GET",
    credentials: "include",
    headers: {
      Authorization: `Bearer ${DEV_BEARER_TOKEN}`,
      Accept: "application/json",
    },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`getPendingSendStatus: ${res.status} ${text}`);
  }
  return (await res.json()) as PendingSendSnapshot;
}
