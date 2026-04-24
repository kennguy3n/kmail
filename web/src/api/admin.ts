/**
 * Typed REST client for the KMail admin surface.
 *
 * The JMAP client in `jmap.ts` talks to the mail / calendar data
 * plane; this module talks to the Tenant Service and DNS
 * Onboarding Service control-plane REST endpoints under
 * `/api/v1/tenants/...`. Keeping them in separate files keeps the
 * JMAP batch machinery from leaking into the (much simpler)
 * admin CRUD path.
 *
 * Authentication mirrors `jmap.ts`: every request carries the
 * dev-bypass bearer token (`Authorization: Bearer kmail-dev`). The
 * Go OIDC middleware accepts that token only when
 * `KMAIL_DEV_BYPASS_TOKEN=kmail-dev` is set on the BFF — in
 * staging / production the middleware rejects it and real KChat
 * OIDC tokens are used instead. See docs/JMAP-CONTRACT.md §3.1.
 *
 * In dev-bypass mode the middleware also reads
 * `X-KMail-Dev-Tenant-Id` off the request so a single bearer token
 * can drive every tenant in the local compose stack
 * (`internal/middleware/auth.go` — `devClaimsFromHeaders`). The
 * admin UI picks a tenant from `GET /api/v1/tenants`, stores the
 * selected ID, and sends it on every tenant-scoped request so the
 * handler-side `checkTenantScope` check accepts the URL tenant ID.
 */
import { DEV_BEARER_TOKEN } from "./jmap";

/** Base path for every control-plane REST route. */
export const ADMIN_API_BASE = "/api/v1";

/**
 * Build the headers for an admin REST request. Mirrors
 * `jmap.ts#authHeaders` so the auth wiring only lives in one
 * conceptual place — the only difference is the optional
 * `X-KMail-Dev-Tenant-Id` header used by the dev-bypass path.
 */
export function adminAuthHeaders(
  tenantId?: string,
  extra: HeadersInit = {},
): Headers {
  const h = new Headers(extra);
  h.set("Authorization", `Bearer ${DEV_BEARER_TOKEN}`);
  if (tenantId) {
    h.set("X-KMail-Dev-Tenant-Id", tenantId);
  }
  return h;
}

/**
 * Thrown for any non-2xx REST response. Carries both the status
 * code and the server-supplied error message (when the BFF returns
 * a JSON body of the shape `{ "error": "<message>" }`).
 */
export class AdminApiError extends Error {
  readonly status: number;
  readonly url: string;
  constructor(url: string, status: number, message: string) {
    super(`${status} ${message}`);
    this.name = "AdminApiError";
    this.status = status;
    this.url = url;
  }
}

async function parseErrorBody(res: Response): Promise<string> {
  const text = await res.text();
  if (!text) return res.statusText;
  try {
    const parsed = JSON.parse(text) as { error?: string };
    if (parsed && typeof parsed.error === "string") return parsed.error;
  } catch {
    // fall through
  }
  return text;
}

async function requestJSON<T>(
  url: string,
  init: RequestInit,
  { expectJson = true }: { expectJson?: boolean } = {},
): Promise<T> {
  const res = await fetch(url, { credentials: "include", ...init });
  if (!res.ok) {
    throw new AdminApiError(url, res.status, await parseErrorBody(res));
  }
  if (!expectJson || res.status === 204) {
    // Matches DELETE endpoints that return 204 No Content.
    return undefined as unknown as T;
  }
  return (await res.json()) as T;
}

/** Mirrors `internal/tenant/service.go#Tenant`. */
export interface Tenant {
  id: string;
  name: string;
  slug: string;
  plan: string;
  status: string;
  created_at: string;
  updated_at: string;
}

/** Mirrors `internal/tenant/service.go#User`. */
export interface TenantUser {
  id: string;
  tenant_id: string;
  kchat_user_id: string;
  stalwart_account_id: string;
  email: string;
  display_name: string;
  role: string;
  status: string;
  quota_bytes: number;
  created_at: string;
  updated_at: string;
}

/** Mirrors `internal/tenant/service.go#Domain`. */
export interface TenantDomain {
  id: string;
  tenant_id: string;
  domain: string;
  verified: boolean;
  mx_verified: boolean;
  spf_verified: boolean;
  dkim_verified: boolean;
  dmarc_verified: boolean;
  created_at: string;
  updated_at: string;
}

/** Mirrors `internal/dns/dns.go#VerificationResult`. */
export interface DomainVerificationResult {
  domain_id: string;
  domain: string;
  mx_verified: boolean;
  spf_verified: boolean;
  dkim_verified: boolean;
  dmarc_verified: boolean;
  verified: boolean;
}

/** One DNS record the tenant must publish. */
export interface DomainRecord {
  type: string;
  name: string;
  value: string;
  ttl?: number;
  priority?: number;
  notes?: string;
}

/** Mirrors `internal/dns/dns.go#DomainRecords`. */
export interface DomainRecords {
  domain: string;
  records: DomainRecord[];
}

/** Mirrors `internal/tenant/service.go#UpdateUserInput` (all fields optional). */
export interface UpdateUserInput {
  display_name?: string;
  role?: string;
  status?: string;
  quota_bytes?: number;
}

/** List every tenant in the control plane (admin-only, bypasses RLS). */
export async function listTenants(): Promise<Tenant[]> {
  return requestJSON<Tenant[]>(`${ADMIN_API_BASE}/tenants`, {
    method: "GET",
    headers: adminAuthHeaders(undefined, { Accept: "application/json" }),
  });
}

/** List domains owned by a tenant. */
export async function listDomains(tenantId: string): Promise<TenantDomain[]> {
  return requestJSON<TenantDomain[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/domains`,
    {
      method: "GET",
      headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
    },
  );
}

/**
 * Run the DNS checks for a single domain and persist the new per-
 * record verification flags. Returns the aggregate result.
 */
export async function verifyDomain(
  tenantId: string,
  domainId: string,
): Promise<DomainVerificationResult> {
  const url = `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/domains/${encodeURIComponent(domainId)}/verify`;
  return requestJSON<DomainVerificationResult>(url, {
    method: "POST",
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

/**
 * Return the set of DNS records the tenant must publish for the
 * domain (MX / SPF / DKIM / DMARC / MTA-STS / TLS-RPT / autoconfig).
 */
export async function getDomainRecords(
  tenantId: string,
  domainId: string,
): Promise<DomainRecords> {
  const url = `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/domains/${encodeURIComponent(domainId)}/dns-records`;
  return requestJSON<DomainRecords>(url, {
    method: "GET",
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

/** List every user in a tenant. */
export async function listUsers(tenantId: string): Promise<TenantUser[]> {
  return requestJSON<TenantUser[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/users`,
    {
      method: "GET",
      headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
    },
  );
}

/**
 * Patch one or more mutable user fields. The Go handler accepts
 * both PUT and PATCH because the input type carries pointer fields
 * — omitted fields are left unchanged on both verbs. We use PATCH
 * here to match the HTTP convention callers expect from a partial
 * update.
 */
export async function updateUser(
  tenantId: string,
  userId: string,
  input: UpdateUserInput,
): Promise<TenantUser> {
  const url = `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/users/${encodeURIComponent(userId)}`;
  return requestJSON<TenantUser>(url, {
    method: "PATCH",
    headers: adminAuthHeaders(tenantId, {
      Accept: "application/json",
      "Content-Type": "application/json",
    }),
    body: JSON.stringify(input),
  });
}

/** Delete a user. Returns on 204 No Content. */
export async function deleteUser(
  tenantId: string,
  userId: string,
): Promise<void> {
  const url = `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/users/${encodeURIComponent(userId)}`;
  await requestJSON<void>(
    url,
    {
      method: "DELETE",
      headers: adminAuthHeaders(tenantId),
    },
    { expectJson: false },
  );
}
