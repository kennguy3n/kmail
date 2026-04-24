/**
 * Typed client for the BFF admin REST surface under
 * `/api/v1/tenants/...`.
 *
 * The admin console (Phase 3) calls these methods to manage
 * tenants, users, domains, shared inboxes, and the audit log.
 * The shapes mirror the Go types in `internal/tenant/service.go`,
 * `internal/dns/dns.go`, and `internal/audit/audit.go`.
 */

import type {
  AuditLogEntry,
  AuditLogQuery,
  DnsRecord,
  Domain,
  SharedInbox,
  Tenant,
  TenantPatch,
  User,
  UserPatch,
  VerifyDomainResult,
} from "../types";
import { DEV_BEARER_TOKEN } from "./jmap";

/** Base URL for every admin REST endpoint the BFF exposes. */
export const ADMIN_API_BASE = "/api/v1";

/**
 * Build the auth headers admin requests need. Mirrors the pattern
 * used by the JMAP client so both surfaces stay in sync when the
 * dev-bypass path gets retired.
 */
function authHeaders(extra: HeadersInit = {}): Headers {
  const h = new Headers(extra);
  h.set("Authorization", `Bearer ${DEV_BEARER_TOKEN}`);
  return h;
}

/**
 * Thin typed wrapper around `fetch` that encodes JSON bodies,
 * decodes JSON responses, and raises on non-2xx status with the
 * server's error message when available.
 */
async function call<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers = authHeaders({ Accept: "application/json" });
  const init: RequestInit = { method, headers };
  if (body !== undefined) {
    headers.set("Content-Type", "application/json");
    init.body = JSON.stringify(body);
  }
  const res = await fetch(`${ADMIN_API_BASE}${path}`, init);
  if (!res.ok) {
    let msg = `${method} ${path}: HTTP ${res.status}`;
    try {
      const payload = (await res.json()) as { error?: string };
      if (payload?.error) msg = payload.error;
    } catch {
      // Body was not JSON; fall through with the default message.
    }
    throw new Error(msg);
  }
  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

// ----------------------------------------------------------------
// Tenants
// ----------------------------------------------------------------

export function getTenant(tenantId: string): Promise<Tenant> {
  return call<Tenant>("GET", `/tenants/${encodeURIComponent(tenantId)}`);
}

export function updateTenant(
  tenantId: string,
  patch: TenantPatch,
): Promise<Tenant> {
  return call<Tenant>(
    "PATCH",
    `/tenants/${encodeURIComponent(tenantId)}`,
    patch,
  );
}

// ----------------------------------------------------------------
// Users
// ----------------------------------------------------------------

export function listUsers(tenantId: string): Promise<User[]> {
  return call<User[]>(
    "GET",
    `/tenants/${encodeURIComponent(tenantId)}/users`,
  );
}

export function getUser(tenantId: string, userId: string): Promise<User> {
  return call<User>(
    "GET",
    `/tenants/${encodeURIComponent(tenantId)}/users/${encodeURIComponent(userId)}`,
  );
}

export interface CreateUserInput {
  email: string;
  displayName: string;
  role?: string;
  status?: string;
  mailboxQuotaBytes?: number;
}

export function createUser(
  tenantId: string,
  input: CreateUserInput,
): Promise<User> {
  return call<User>(
    "POST",
    `/tenants/${encodeURIComponent(tenantId)}/users`,
    input,
  );
}

export function updateUser(
  tenantId: string,
  userId: string,
  patch: UserPatch,
): Promise<User> {
  return call<User>(
    "PATCH",
    `/tenants/${encodeURIComponent(tenantId)}/users/${encodeURIComponent(userId)}`,
    patch,
  );
}

export function deleteUser(tenantId: string, userId: string): Promise<void> {
  return call<void>(
    "DELETE",
    `/tenants/${encodeURIComponent(tenantId)}/users/${encodeURIComponent(userId)}`,
  );
}

// ----------------------------------------------------------------
// Domains
// ----------------------------------------------------------------

export function listDomains(tenantId: string): Promise<Domain[]> {
  return call<Domain[]>(
    "GET",
    `/tenants/${encodeURIComponent(tenantId)}/domains`,
  );
}

export interface CreateDomainInput {
  domain: string;
}

export function createDomain(
  tenantId: string,
  input: CreateDomainInput,
): Promise<Domain> {
  return call<Domain>(
    "POST",
    `/tenants/${encodeURIComponent(tenantId)}/domains`,
    input,
  );
}

export function verifyDomain(
  tenantId: string,
  domainId: string,
): Promise<VerifyDomainResult> {
  return call<VerifyDomainResult>(
    "POST",
    `/tenants/${encodeURIComponent(tenantId)}/domains/${encodeURIComponent(domainId)}/verify`,
  );
}

export function getDnsRecords(
  tenantId: string,
  domainId: string,
): Promise<DnsRecord[]> {
  return call<DnsRecord[]>(
    "GET",
    `/tenants/${encodeURIComponent(tenantId)}/domains/${encodeURIComponent(domainId)}/dns-records`,
  );
}

// ----------------------------------------------------------------
// Shared inboxes
// ----------------------------------------------------------------

export function listSharedInboxes(tenantId: string): Promise<SharedInbox[]> {
  return call<SharedInbox[]>(
    "GET",
    `/tenants/${encodeURIComponent(tenantId)}/shared-inboxes`,
  );
}

export interface CreateSharedInboxInput {
  address: string;
  displayName: string;
}

export function createSharedInbox(
  tenantId: string,
  input: CreateSharedInboxInput,
): Promise<SharedInbox> {
  return call<SharedInbox>(
    "POST",
    `/tenants/${encodeURIComponent(tenantId)}/shared-inboxes`,
    input,
  );
}

// ----------------------------------------------------------------
// Audit log
// ----------------------------------------------------------------

export async function getAuditLog(
  tenantId: string,
  filters: AuditLogQuery = {},
): Promise<{ entries: AuditLogEntry[] }> {
  const params = new URLSearchParams();
  if (filters.action) params.set("action", filters.action);
  if (filters.actor) params.set("actor", filters.actor);
  if (filters.resource) params.set("resource", filters.resource);
  if (filters.since) params.set("since", filters.since);
  if (filters.until) params.set("until", filters.until);
  if (filters.limit) params.set("limit", String(filters.limit));
  if (filters.offset) params.set("offset", String(filters.offset));
  const qs = params.toString();
  const path =
    `/tenants/${encodeURIComponent(tenantId)}/audit-log` +
    (qs ? `?${qs}` : "");
  return call<{ entries: AuditLogEntry[] }>("GET", path);
}
