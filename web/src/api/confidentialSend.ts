/**
 * Typed client for the Phase 5 Confidential Send portal. Lives
 * outside `admin.ts` because the public-portal endpoints
 * (`GET /api/v1/secure/{token}`) are intentionally unauthenticated
 * — token + password are the only credentials. The tenant-scoped
 * `create / list / revoke` routes still flow through the standard
 * admin auth headers.
 */
import { adminAuthHeaders, ADMIN_API_BASE, AdminApiError } from "./admin";

export interface SecureMessage {
  id: string;
  tenant_id: string;
  sender_id: string;
  link_token: string;
  encrypted_blob_ref?: string;
  has_password: boolean;
  expires_at: string;
  max_views: number;
  view_count: number;
  revoked: boolean;
  created_at: string;
}

export interface CreateSecureMessageInput {
  tenantId: string;
  senderId: string;
  encryptedBlobRef: string;
  password?: string;
  expiresInSeconds: number;
  maxViews: number;
}

async function requestJSON<T>(url: string, init: RequestInit): Promise<T> {
  const res = await fetch(url, init);
  if (!res.ok) {
    let body: { error?: string } = {};
    try {
      body = (await res.json()) as { error?: string };
    } catch {
      // ignore
    }
    throw new AdminApiError(
      url,
      res.status,
      body.error ?? res.statusText ?? "request failed",
    );
  }
  return (await res.json()) as T;
}

export async function createSecureMessage(
  input: CreateSecureMessageInput,
): Promise<SecureMessage> {
  return requestJSON<SecureMessage>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(input.tenantId)}/confidential-send`,
    {
      method: "POST",
      headers: adminAuthHeaders(input.tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        sender_id: input.senderId,
        encrypted_blob_ref: input.encryptedBlobRef,
        password: input.password ?? "",
        expires_in_seconds: input.expiresInSeconds,
        max_views: input.maxViews,
      }),
    },
  );
}

export async function listSecureMessages(
  tenantId: string,
  senderId?: string,
): Promise<SecureMessage[]> {
  const url = senderId
    ? `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/confidential-send?sender_id=${encodeURIComponent(senderId)}`
    : `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/confidential-send`;
  return requestJSON<SecureMessage[]>(url, {
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

export async function revokeSecureLink(
  tenantId: string,
  linkId: string,
): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/confidential-send/${encodeURIComponent(linkId)}`,
    { method: "DELETE", headers: adminAuthHeaders(tenantId) },
  );
}

/**
 * Public-portal lookup for the recipient. No auth headers — the
 * token + (optional) password are the only credentials. POSTs the
 * password when supplied; the BFF accepts both methods so the
 * portal can do a "do I need a password?" probe before prompting.
 */
export async function getSecureMessage(
  token: string,
  password?: string,
): Promise<SecureMessage> {
  const url = `${ADMIN_API_BASE}/secure/${encodeURIComponent(token)}`;
  const init: RequestInit = password
    ? {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ password }),
      }
    : { method: "GET", headers: { Accept: "application/json" } };
  return requestJSON<SecureMessage>(url, init);
}
