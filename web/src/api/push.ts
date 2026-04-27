/**
 * Typed client for the KMail Push Service. Wraps
 * `/api/v1/push/...` so the UI code never has to stringify
 * JSON / manage fetch options by hand.
 */

import { ADMIN_API_BASE, adminAuthHeaders, requestJSON } from "./admin";

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
    headers: adminAuthHeaders(undefined, {
      Accept: "application/json",
      "Content-Type": "application/json",
    }),
    body: JSON.stringify(input),
  });
}

export async function unsubscribe(subscriptionId: string): Promise<void> {
  await requestJSON<void>(
    `${ADMIN_API_BASE}/push/subscriptions/${encodeURIComponent(subscriptionId)}`,
    { method: "DELETE", headers: adminAuthHeaders() },
    { expectJson: false },
  );
}

export async function listSubscriptions(): Promise<PushSubscription[]> {
  const res = await requestJSON<{ subscriptions: PushSubscription[] }>(
    `${ADMIN_API_BASE}/push/subscriptions`,
    { method: "GET", headers: adminAuthHeaders(undefined, { Accept: "application/json" }) },
  );
  return res.subscriptions ?? [];
}

export async function getPreferences(): Promise<NotificationPreference> {
  return requestJSON<NotificationPreference>(`${ADMIN_API_BASE}/push/preferences`, {
    method: "GET",
    headers: adminAuthHeaders(undefined, { Accept: "application/json" }),
  });
}

export async function updatePreferences(
  prefs: Partial<NotificationPreference>,
): Promise<NotificationPreference> {
  return requestJSON<NotificationPreference>(`${ADMIN_API_BASE}/push/preferences`, {
    method: "PUT",
    headers: adminAuthHeaders(undefined, {
      Accept: "application/json",
      "Content-Type": "application/json",
    }),
    body: JSON.stringify(prefs),
  });
}

/**
 * Register the browser's Push API subscription with KMail's Web
 * Push transport (Phase 8 GA-readiness).
 *
 * The caller supplies the active service worker registration and
 * the application server's VAPID public key (typically delivered
 * to the page through a server-rendered config or the `/api/v1/auth/me`
 * envelope). The helper:
 *
 *   1. Asks the browser for permission to display notifications.
 *   2. Subscribes to the platform push service via
 *      `pushManager.subscribe({ userVisibleOnly: true, applicationServerKey })`.
 *   3. Posts the resulting endpoint + p256dh + auth keys to
 *      `/api/v1/push/subscribe`.
 *
 * Returns the persisted KMail subscription row.
 */
export async function registerWebPush(
  registration: ServiceWorkerRegistration,
  vapidPublicKey: string,
): Promise<PushSubscription> {
  if (typeof Notification === "undefined") {
    throw new Error("Notifications API unavailable in this browser");
  }
  const permission = await Notification.requestPermission();
  if (permission !== "granted") {
    throw new Error("Notification permission denied");
  }
  // The lib.dom.d.ts type for `applicationServerKey` is
  // `BufferSource`, which the TS compiler narrows to require an
  // ArrayBuffer (not a SharedArrayBuffer-backed view). Construct
  // the buffer view explicitly so the type-narrowing succeeds.
  const keyBytes = urlBase64ToUint8Array(vapidPublicKey);
  const keyBuf = new ArrayBuffer(keyBytes.byteLength);
  new Uint8Array(keyBuf).set(keyBytes);
  const browserSub = await registration.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: keyBuf,
  });
  const json = browserSub.toJSON();
  return subscribe({
    device_type: "web",
    push_endpoint: json.endpoint ?? browserSub.endpoint,
    p256dh_key: json.keys?.p256dh,
    auth_key: json.keys?.auth,
  });
}

/**
 * Decode a base64url-encoded VAPID public key into the Uint8Array
 * shape `pushManager.subscribe` expects.
 */
function urlBase64ToUint8Array(base64String: string): Uint8Array {
  const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
  const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
  const rawData = atob(base64);
  const output = new Uint8Array(rawData.length);
  for (let i = 0; i < rawData.length; i++) output[i] = rawData.charCodeAt(i);
  return output;
}
