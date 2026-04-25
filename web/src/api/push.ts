/**
 * Typed client for the KMail Push Service. Wraps
 * `/api/v1/push/...` so the UI code never has to stringify
 * JSON / manage fetch options by hand.
 */

import { ADMIN_API_BASE, requestJSON } from "./admin";

export interface PushSubscription {
  id: string;
  tenant_id: string;
  user_id: string;
  device_type: "web" | "ios" | "android";
  push_endpoint: string;
  auth_key?: string;
  p256dh_key?: string;
  created_at: string;
}

export interface NotificationPreference {
  tenant_id: string;
  user_id: string;
  new_email: boolean;
  calendar_reminder: boolean;
  shared_inbox: boolean;
  quiet_hours_start: string;
  quiet_hours_end: string;
  updated_at: string;
}

export async function subscribe(input: Partial<PushSubscription>): Promise<PushSubscription> {
  return requestJSON<PushSubscription>(`${ADMIN_API_BASE}/push/subscribe`, {
    method: "POST",
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
}

export async function unsubscribe(subscriptionId: string): Promise<void> {
  await fetch(`${ADMIN_API_BASE}/push/subscriptions/${encodeURIComponent(subscriptionId)}`, {
    method: "DELETE",
  });
}

export async function listSubscriptions(): Promise<PushSubscription[]> {
  const res = await requestJSON<{ subscriptions: PushSubscription[] }>(
    `${ADMIN_API_BASE}/push/subscriptions`,
    { method: "GET", headers: { Accept: "application/json" } },
  );
  return res.subscriptions ?? [];
}

export async function getPreferences(): Promise<NotificationPreference> {
  return requestJSON<NotificationPreference>(`${ADMIN_API_BASE}/push/preferences`, {
    method: "GET",
    headers: { Accept: "application/json" },
  });
}

export async function updatePreferences(
  prefs: Partial<NotificationPreference>,
): Promise<NotificationPreference> {
  return requestJSON<NotificationPreference>(`${ADMIN_API_BASE}/push/preferences`, {
    method: "PUT",
    headers: { Accept: "application/json", "Content-Type": "application/json" },
    body: JSON.stringify(prefs),
  });
}
