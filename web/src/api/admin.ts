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

export async function requestJSON<T>(
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
  /** "user" = paid seat, "shared_inbox" / "service" = excluded from seat count */
  account_type?: string;
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

/**
 * Mirrors `internal/audit/audit.go#Entry`. Each row carries a
 * hash-chain link (`prev_hash`, `entry_hash`) so `VerifyChain`
 * can detect tampering.
 */
export interface AuditLogEntry {
  id: string;
  tenant_id: string;
  actor_id: string;
  actor_type: "user" | "admin" | "system";
  action: string;
  resource_type: string;
  resource_id: string;
  metadata: Record<string, unknown> | null;
  ip_address: string;
  user_agent: string;
  prev_hash: string;
  entry_hash: string;
  created_at: string;
}

/** Filters accepted by the audit-log paginated query. */
export interface AuditLogQuery {
  action?: string;
  actor?: string;
  resource_type?: string;
  resource_id?: string;
  since?: string;
  until?: string;
  limit?: number;
  offset?: number;
}

function auditQueryString(q?: AuditLogQuery): string {
  if (!q) return "";
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) {
    if (v === undefined || v === null || v === "") continue;
    params.set(k, String(v));
  }
  const s = params.toString();
  return s ? `?${s}` : "";
}

/**
 * Paginated query of the audit log. Backend route:
 * `GET /api/v1/tenants/{id}/audit-log`.
 *
 * The Go handler (`internal/audit/handlers.go#query`) wraps the
 * rows in a `{ "entries": [...] }` envelope, so we unwrap here
 * and expose the bare array to callers.
 */
export async function getAuditLog(
  tenantId: string,
  filters?: AuditLogQuery,
): Promise<AuditLogEntry[]> {
  const url =
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/audit-log` +
    auditQueryString(filters);
  const body = await requestJSON<{ entries?: AuditLogEntry[] }>(url, {
    method: "GET",
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
  return body.entries ?? [];
}

/**
 * Export the audit log as JSON or CSV. Returns the raw response
 * body as a string so the caller can trigger a file download.
 */
export async function exportAuditLog(
  tenantId: string,
  format: "json" | "csv" = "json",
  range?: { since?: string; until?: string },
): Promise<string> {
  const params = new URLSearchParams({ format });
  if (range?.since) params.set("since", range.since);
  if (range?.until) params.set("until", range.until);
  const url =
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/audit-log/export?` +
    params.toString();
  const res = await fetch(url, {
    method: "GET",
    credentials: "include",
    headers: adminAuthHeaders(tenantId),
  });
  if (!res.ok) {
    throw new AdminApiError(url, res.status, await parseErrorBody(res));
  }
  return res.text();
}

/**
 * Verify the hash chain. Returns `{ ok: true }` when the full
 * chain validates; the backend returns HTTP 409 with an `error`
 * body when a tamper is detected, which this helper surfaces as
 * `{ ok: false, error }` so callers don't need a try/catch for
 * the expected "chain broken" outcome.
 */
export async function verifyAuditChain(
  tenantId: string,
): Promise<{ ok: boolean; error?: string }> {
  const url = `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/audit-log/verify`;
  const res = await fetch(url, {
    method: "POST",
    credentials: "include",
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
  if (res.ok) {
    return { ok: true };
  }
  if (res.status === 409) {
    const body = (await res.json().catch(() => ({}))) as { error?: string };
    return { ok: false, error: body.error ?? "audit chain broken" };
  }
  throw new AdminApiError(url, res.status, await parseErrorBody(res));
}

// ---------------------------------------------------------------
// Billing / Quota
// ---------------------------------------------------------------

/** Mirrors `internal/billing/billing.go#Quota`. */
export interface Quota {
  tenant_id: string;
  storage_used_bytes: number;
  storage_limit_bytes: number;
  seat_count: number;
  seat_limit: number;
  updated_at?: string;
}

/** Mirrors `internal/billing/billing.go#BillingSummary`. */
export interface BillingSummary {
  tenant_id: string;
  plan: string;
  seat_count: number;
  seat_limit: number;
  storage_used_bytes: number;
  storage_limit_bytes: number;
  per_seat_cents: number;
  monthly_total_cents: number;
  currency: string;
}

export interface UpdateQuotaLimitsInput {
  storage_limit_bytes?: number;
  seat_limit?: number;
}

export async function getBillingSummary(tenantId: string): Promise<BillingSummary> {
  return requestJSON<BillingSummary>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/billing`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function getQuota(tenantId: string): Promise<Quota> {
  return requestJSON<Quota>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/billing/usage`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function updateQuotaLimits(
  tenantId: string,
  input: UpdateQuotaLimitsInput,
): Promise<Quota> {
  return requestJSON<Quota>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/billing`,
    {
      method: "PATCH",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify(input),
    },
  );
}

// ---------------------------------------------------------------
// DMARC reports
// ---------------------------------------------------------------

export interface DMARCReport {
  id: string;
  tenant_id: string;
  domain_id?: string;
  report_id: string;
  org_name: string;
  email: string;
  date_range_begin: string;
  date_range_end: string;
  domain: string;
  adkim: string;
  aspf: string;
  policy: string;
  pass_count: number;
  fail_count: number;
  records: unknown;
  created_at: string;
}

export interface DMARCSummary {
  tenant_id: string;
  domain_id?: string;
  domain: string;
  pass_count: number;
  fail_count: number;
  total: number;
  pass_rate: number;
  report_count: number;
  window_days: number;
}

export async function listDmarcReports(
  tenantId: string,
  opts: { domainId?: string; limit?: number; offset?: number } = {},
): Promise<DMARCReport[]> {
  const params = new URLSearchParams();
  if (opts.domainId) params.set("domain_id", opts.domainId);
  if (opts.limit !== undefined) params.set("limit", String(opts.limit));
  if (opts.offset !== undefined) params.set("offset", String(opts.offset));
  const q = params.toString() ? `?${params.toString()}` : "";
  return requestJSON<DMARCReport[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/dmarc-reports${q}`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function getDmarcSummary(
  tenantId: string,
  domainId?: string,
): Promise<DMARCSummary> {
  const q = domainId ? `?domain_id=${encodeURIComponent(domainId)}` : "";
  return requestJSON<DMARCSummary>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/dmarc-reports/summary${q}`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function uploadDmarcReport(
  tenantId: string,
  xml: string,
): Promise<DMARCReport> {
  return requestJSON<DMARCReport>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/dmarc-reports`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/xml",
      }),
      body: xml,
    },
  );
}

// ---------------------------------------------------------------
// DNS wizard
// ---------------------------------------------------------------

/** One step in the DNS wizard walkthrough. */
export interface DnsWizardStep {
  key: "mx" | "spf" | "dkim" | "dmarc" | "mta_sts" | "tls_rpt" | "autoconfig";
  label: string;
  record: DomainRecord | null;
  verified: boolean;
  errorMessage?: string;
}

/** Summary state for the DNS wizard. */
export interface DnsWizardStatus {
  steps: DnsWizardStep[];
  allVerified: boolean;
}

// Which backend flags map to which wizard step.
const WIZARD_STEP_LABELS: Record<DnsWizardStep["key"], string> = {
  mx: "MX records",
  spf: "SPF (TXT)",
  dkim: "DKIM",
  dmarc: "DMARC",
  mta_sts: "MTA-STS",
  tls_rpt: "TLS-RPT",
  autoconfig: "Autoconfig",
};

function pickRecord(
  records: DomainRecord[],
  key: DnsWizardStep["key"],
): DomainRecord | null {
  switch (key) {
    case "mx":
      return records.find((r) => r.type === "MX") ?? null;
    case "spf":
      return records.find((r) => r.type === "TXT" && r.value.startsWith("v=spf1")) ?? null;
    case "dkim":
      return records.find((r) => r.name.includes("._domainkey.")) ?? null;
    case "dmarc":
      return records.find((r) => r.name.startsWith("_dmarc.")) ?? null;
    case "mta_sts":
      return records.find((r) => r.name.startsWith("_mta-sts.") || r.name.startsWith("mta-sts.")) ?? null;
    case "tls_rpt":
      return records.find((r) => r.name.startsWith("_smtp._tls.")) ?? null;
    case "autoconfig":
      return records.find((r) => r.name.startsWith("autoconfig.") || r.name.startsWith("autodiscover.")) ?? null;
  }
}

/**
 * Fetch the DNS wizard status for a given domain by composing the
 * existing records + verification endpoints. The backend only
 * surfaces per-check booleans for MX/SPF/DKIM/DMARC today; the
 * MTA-STS / TLS-RPT / autoconfig steps render the expected records
 * and rely on the tenant to verify manually.
 */
export async function getDnsWizardStatus(
  tenantId: string,
  domainId: string,
): Promise<DnsWizardStatus> {
  const [records, verification] = await Promise.all([
    getDomainRecords(tenantId, domainId),
    verifyDomain(tenantId, domainId),
  ]);
  const flags: Record<DnsWizardStep["key"], boolean> = {
    mx: verification.mx_verified,
    spf: verification.spf_verified,
    dkim: verification.dkim_verified,
    dmarc: verification.dmarc_verified,
    mta_sts: false,
    tls_rpt: false,
    autoconfig: false,
  };
  const steps: DnsWizardStep[] = (Object.keys(WIZARD_STEP_LABELS) as DnsWizardStep["key"][]).map(
    (key) => ({
      key,
      label: WIZARD_STEP_LABELS[key],
      record: pickRecord(records.records ?? [], key),
      verified: flags[key],
    }),
  );
  return {
    steps,
    allVerified: verification.verified,
  };
}

// ---------------------------------------------------------------
// Migration wizard
// ---------------------------------------------------------------

export interface MigrationJob {
  id: string;
  tenant_id: string;
  source_type: string;
  source_host: string;
  source_user: string;
  destination_user_id: string;
  status: "pending" | "running" | "paused" | "completed" | "failed" | "cancelled";
  messages_total: number;
  messages_synced: number;
  error_message?: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
}

export interface CreateMigrationJobInput {
  source_type: "gmail_imap" | "generic_imap" | "ms365_imap";
  source_host: string;
  source_port?: number;
  source_user: string;
  source_password: string;
  destination_user_id: string;
}

export async function listMigrationJobs(tenantId: string): Promise<MigrationJob[]> {
  const res = await requestJSON<{ jobs: MigrationJob[] }>(
    `${ADMIN_API_BASE}/migrations`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
  return res.jobs ?? [];
}

export async function createMigrationJob(
  tenantId: string,
  input: CreateMigrationJobInput,
): Promise<MigrationJob> {
  return requestJSON<MigrationJob>(`${ADMIN_API_BASE}/migrations`, {
    method: "POST",
    headers: adminAuthHeaders(tenantId, {
      Accept: "application/json",
      "Content-Type": "application/json",
    }),
    body: JSON.stringify(input),
  });
}

export async function getMigrationJob(
  tenantId: string,
  jobId: string,
): Promise<MigrationJob> {
  return requestJSON<MigrationJob>(
    `${ADMIN_API_BASE}/migrations/${encodeURIComponent(jobId)}`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function pauseMigrationJob(tenantId: string, jobId: string): Promise<void> {
  await requestJSON<void>(
    `${ADMIN_API_BASE}/migrations/${encodeURIComponent(jobId)}/pause`,
    { method: "POST", headers: adminAuthHeaders(tenantId) },
    { expectJson: false },
  );
}

export async function resumeMigrationJob(tenantId: string, jobId: string): Promise<void> {
  await requestJSON<void>(
    `${ADMIN_API_BASE}/migrations/${encodeURIComponent(jobId)}/resume`,
    { method: "POST", headers: adminAuthHeaders(tenantId) },
    { expectJson: false },
  );
}

export async function cancelMigrationJob(tenantId: string, jobId: string): Promise<void> {
  await requestJSON<void>(
    `${ADMIN_API_BASE}/migrations/${encodeURIComponent(jobId)}`,
    { method: "DELETE", headers: adminAuthHeaders(tenantId) },
    { expectJson: false },
  );
}

/**
 * Test the IMAP credentials supplied to the migration wizard before
 * the operator commits to creating an import job. The backend
 * always returns 200; success is signalled by `ok: true`. Network
 * errors (DNS, connect, TLS) and IMAP NO/BAD responses come back as
 * `{ ok: false, error: "<detail>" }`.
 */
export interface TestMigrationConnectionInput {
  host: string;
  port: number;
  username: string;
  password: string;
  use_tls: boolean;
}

export interface TestMigrationConnectionResult {
  ok: boolean;
  error?: string;
}

export async function testMigrationConnection(
  tenantId: string,
  input: TestMigrationConnectionInput,
): Promise<TestMigrationConnectionResult> {
  return requestJSON<TestMigrationConnectionResult>(
    `${ADMIN_API_BASE}/migrations/test-connection`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify(input),
    },
  );
}

// ---------------------------------------------------------------
// Pricing / plan management
// ---------------------------------------------------------------

/** Plan IDs accepted by `PATCH /api/v1/tenants/{id}/billing/plan`. */
export type BillingPlan = "core" | "pro" | "privacy";

/** Static plan catalog rendered by the pricing admin page. */
export interface PlanCatalogEntry {
  id: BillingPlan;
  name: string;
  monthlyPriceCents: number;
  dailySendLimit: number;
  storagePerSeatGB: number;
  features: string[];
}

export const PLAN_CATALOG: PlanCatalogEntry[] = [
  {
    id: "core",
    name: "KChat Core Email",
    monthlyPriceCents: 300,
    dailySendLimit: 500,
    storagePerSeatGB: 5,
    features: [
      "Standard Private Mail",
      "Basic spam filtering",
      "IMAP / SMTP / JMAP access",
      "DNS onboarding wizard",
    ],
  },
  {
    id: "pro",
    name: "KChat Mail Pro",
    monthlyPriceCents: 600,
    dailySendLimit: 2000,
    storagePerSeatGB: 15,
    features: [
      "Everything in Core",
      "Advanced spam + DNSBL",
      "CalDAV / CardDAV calendars + contacts",
      "Shared inboxes (no paid seats)",
      "Migration automation",
    ],
  },
  {
    id: "privacy",
    name: "KChat Privacy",
    monthlyPriceCents: 900,
    dailySendLimit: 5000,
    storagePerSeatGB: 50,
    features: [
      "Everything in Pro",
      "Confidential Send (StrictZK)",
      "Zero-Access Vault",
      "Dedicated IP pool",
      "DMARC reporting",
    ],
  },
];

export async function changePlan(
  tenantId: string,
  plan: BillingPlan,
): Promise<BillingSummary> {
  return requestJSON<BillingSummary>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/billing/plan`,
    {
      method: "PATCH",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ plan }),
    },
  );
}

// ---------------------------------------------------------------
// IP reputation
// ---------------------------------------------------------------

export interface IPReputationMetrics {
  ip_id: string;
  address: string;
  pool_id: string;
  pool_name: string;
  pool_type: string;
  reputation_score: number;
  daily_volume: number;
  bounce_rate: number;
  complaint_rate: number;
  status: string;
  warmup_day: number;
  updated_at: string;
}

export interface IPReputationHistoryPoint {
  day: string;
  reputation_score: number;
  daily_volume: number;
}

export async function listIpReputation(): Promise<IPReputationMetrics[]> {
  const res = await requestJSON<{ ips: IPReputationMetrics[] }>(
    `${ADMIN_API_BASE}/admin/ip-reputation`,
    { method: "GET", headers: adminAuthHeaders(undefined, { Accept: "application/json" }) },
  );
  return res.ips ?? [];
}

export async function getIpReputationHistory(
  ipId: string,
): Promise<IPReputationHistoryPoint[]> {
  const res = await requestJSON<{ history: IPReputationHistoryPoint[] }>(
    `${ADMIN_API_BASE}/admin/ip-reputation/${encodeURIComponent(ipId)}/history`,
    { method: "GET", headers: adminAuthHeaders(undefined, { Accept: "application/json" }) },
  );
  return res.history ?? [];
}

// ---------------------------------------------------------------
// Deliverability alerts
// ---------------------------------------------------------------

export interface DeliverabilityAlert {
  id: string;
  tenant_id: string;
  alert_type: string;
  severity: "info" | "warning" | "critical";
  metric_name: string;
  metric_value: number;
  threshold_value: number;
  message: string;
  acknowledged: boolean;
  created_at: string;
}

export interface AlertThreshold {
  tenant_id: string;
  metric_name: string;
  warning_threshold: number;
  critical_threshold: number;
  updated_at: string;
}

export async function listDeliverabilityAlerts(tenantId: string): Promise<DeliverabilityAlert[]> {
  const res = await requestJSON<{ alerts: DeliverabilityAlert[] }>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/deliverability/alerts`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
  return res.alerts ?? [];
}

export async function ackDeliverabilityAlert(tenantId: string, alertId: string): Promise<void> {
  await requestJSON<void>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/deliverability/alerts/${encodeURIComponent(alertId)}/acknowledge`,
    { method: "POST", headers: adminAuthHeaders(tenantId) },
    { expectJson: false },
  );
}

export async function listAlertThresholds(tenantId: string): Promise<AlertThreshold[]> {
  const res = await requestJSON<{ thresholds: AlertThreshold[] }>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/deliverability/thresholds`,
    { method: "GET", headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
  return res.thresholds ?? [];
}

export async function updateAlertThresholds(
  tenantId: string,
  thresholds: AlertThreshold[],
): Promise<AlertThreshold[]> {
  const res = await requestJSON<{ thresholds: AlertThreshold[] }>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/deliverability/thresholds`,
    {
      method: "PUT",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ thresholds }),
    },
  );
  return res.thresholds ?? [];
}

// =====================================================================
// Phase 4 — Availability SLO dashboard
// =====================================================================

export interface SLOAvailability {
  tenant_id: string;
  window_seconds: number;
  total: number;
  successes: number;
  failures: number;
  availability: number;
  target: number;
}
export interface SLOLatency {
  tenant_id: string;
  window_seconds: number;
  count: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
}
export interface SLOResponse {
  availability: SLOAvailability;
  latency: SLOLatency;
}
export interface SLOBreach {
  tenant_id: string;
  started_at: string;
  ended_at: string;
  availability: number;
  target: number;
}

export async function getSloOverview(): Promise<SLOResponse> {
  return requestJSON<SLOResponse>(`${ADMIN_API_BASE}/admin/slo`, {
    headers: adminAuthHeaders(undefined, { Accept: "application/json" }),
  });
}
export async function getTenantSlo(tenantId: string): Promise<SLOResponse> {
  return requestJSON<SLOResponse>(
    `${ADMIN_API_BASE}/admin/slo/${encodeURIComponent(tenantId)}`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}
export async function getSloBreaches(tenantId?: string): Promise<SLOBreach[]> {
  const url = tenantId
    ? `${ADMIN_API_BASE}/admin/slo/breaches?tenant_id=${encodeURIComponent(tenantId)}`
    : `${ADMIN_API_BASE}/admin/slo/breaches`;
  return requestJSON<SLOBreach[]>(url, {
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

// =====================================================================
// Phase 5 — Storage placement controls
// =====================================================================

export interface PlacementPolicy {
  tenant_id: string;
  policy_ref: string;
  countries: string[];
  preferred_provider?: string;
  encryption_mode: string;
  erasure_profile?: string;
  updated_at?: string;
}
export interface AvailableRegion { code: string; name: string }

export async function getPlacementPolicy(tenantId: string): Promise<PlacementPolicy> {
  return requestJSON<PlacementPolicy>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/storage/placement`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}
export async function updatePlacementPolicy(
  tenantId: string,
  policy: PlacementPolicy,
): Promise<PlacementPolicy> {
  return requestJSON<PlacementPolicy>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/storage/placement`,
    {
      method: "PUT",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify(policy),
    },
  );
}
export async function listRegions(): Promise<AvailableRegion[]> {
  return requestJSON<AvailableRegion[]>(`${ADMIN_API_BASE}/storage/regions`, {
    headers: adminAuthHeaders(undefined, { Accept: "application/json" }),
  });
}

// =====================================================================
// Phase 5 — Retention policies
// =====================================================================

export interface RetentionPolicy {
  id: string;
  tenant_id: string;
  policy_type: "archive" | "delete";
  retention_days: number;
  applies_to: "all" | "mailbox" | "label";
  target_ref?: string;
  enabled: boolean;
  created_at?: string;
  updated_at?: string;
}

export async function listRetentionPolicies(tenantId: string): Promise<RetentionPolicy[]> {
  return requestJSON<RetentionPolicy[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/retention`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}
export async function createRetentionPolicy(tenantId: string, policy: RetentionPolicy): Promise<RetentionPolicy> {
  return requestJSON<RetentionPolicy>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/retention`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify(policy),
    },
  );
}
export async function deleteRetentionPolicy(tenantId: string, id: string): Promise<void> {
  await requestJSON<void>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/retention/${encodeURIComponent(id)}`,
    { method: "DELETE", headers: adminAuthHeaders(tenantId) },
    { expectJson: false },
  );
}

// =====================================================================
// Phase 5 — Approval workflow
// =====================================================================

export interface ApprovalRequest {
  id: string;
  tenant_id: string;
  requester_id: string;
  action: string;
  target_resource: string;
  status: "pending" | "approved" | "rejected" | "expired";
  approver_id?: string;
  reason?: string;
  created_at: string;
  resolved_at?: string;
  expires_at: string;
}

export async function listApprovals(tenantId: string, status?: string): Promise<ApprovalRequest[]> {
  const qs = status ? `?status=${encodeURIComponent(status)}` : "";
  return requestJSON<ApprovalRequest[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/approvals${qs}`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}
export async function approveApprovalRequest(tenantId: string, id: string, approverId: string): Promise<ApprovalRequest> {
  return requestJSON<ApprovalRequest>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/approvals/${encodeURIComponent(id)}/approve`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ approver_id: approverId }),
    },
  );
}
export async function rejectApprovalRequest(tenantId: string, id: string, approverId: string, reason: string): Promise<ApprovalRequest> {
  return requestJSON<ApprovalRequest>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/approvals/${encodeURIComponent(id)}/reject`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ approver_id: approverId, reason }),
    },
  );
}
export async function getApprovalConfig(tenantId: string): Promise<Record<string, boolean>> {
  return requestJSON<Record<string, boolean>>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/approvals/config`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}
export async function setApprovalConfig(tenantId: string, config: Record<string, boolean>): Promise<Record<string, boolean>> {
  return requestJSON<Record<string, boolean>>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/approvals/config`,
    {
      method: "PUT",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify(config),
    },
  );
}

// =====================================================================
// Phase 5 — Tenant data export / eDiscovery
// =====================================================================

export interface ExportJob {
  id: string;
  tenant_id: string;
  requester_id: string;
  format: "mbox" | "eml" | "pst_stub";
  scope: "all" | "mailbox" | "date_range";
  scope_ref?: string;
  status: "pending" | "running" | "completed" | "failed";
  download_url?: string;
  error_message?: string;
  created_at: string;
  started_at?: string;
  completed_at?: string;
}

export async function listExports(tenantId: string): Promise<ExportJob[]> {
  return requestJSON<ExportJob[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/exports`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}
export async function createExport(
  tenantId: string,
  requesterId: string,
  format: ExportJob["format"],
  scope: ExportJob["scope"],
  scopeRef: string,
): Promise<ExportJob> {
  return requestJSON<ExportJob>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/exports`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        requester_id: requesterId,
        format,
        scope,
        scope_ref: scopeRef,
      }),
    },
  );
}

// =====================================================================
// Phase 5 — Zero-Access Vault folders
// =====================================================================

export interface VaultFolder {
  id: string;
  tenant_id: string;
  user_id: string;
  folder_name: string;
  encryption_mode: "StrictZK";
  /** Wrapped DEK (base64url) — server stores opaque bytes only. */
  wrapped_dek?: string;
  key_algorithm: string;
  /** Wrapping nonce (base64url). */
  nonce?: string;
  created_at: string;
  updated_at: string;
}

export async function listVaultFolders(
  tenantId: string,
  userId?: string,
): Promise<VaultFolder[]> {
  const url = userId
    ? `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/vault/folders?user_id=${encodeURIComponent(userId)}`
    : `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/vault/folders`;
  return requestJSON<VaultFolder[]>(url, {
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

export async function createVaultFolder(
  tenantId: string,
  userId: string,
  folderName: string,
): Promise<VaultFolder> {
  return requestJSON<VaultFolder>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/vault/folders`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        user_id: userId,
        folder_name: folderName,
        encryption_mode: "StrictZK",
      }),
    },
  );
}

export async function deleteVaultFolder(
  tenantId: string,
  folderId: string,
): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/vault/folders/${encodeURIComponent(folderId)}`,
    { method: "DELETE", headers: adminAuthHeaders(tenantId) },
  );
}

export async function setVaultFolderEncryptionMeta(
  tenantId: string,
  folderId: string,
  wrappedDek: string,
  keyAlgorithm: string,
  nonce: string,
): Promise<VaultFolder> {
  return requestJSON<VaultFolder>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/vault/folders/${encodeURIComponent(folderId)}/encryption-meta`,
    {
      method: "PUT",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        wrapped_dek: wrappedDek,
        key_algorithm: keyAlgorithm,
        nonce,
      }),
    },
  );
}

// =====================================================================
// Phase 5 — Customer-managed keys (CMK)
// =====================================================================

export interface CmkKey {
  id: string;
  tenant_id: string;
  key_fingerprint: string;
  public_key_pem: string;
  status: "active" | "deprecated" | "revoked";
  algorithm: string;
  created_at: string;
  updated_at: string;
}

export async function listCmkKeys(tenantId: string): Promise<CmkKey[]> {
  return requestJSON<CmkKey[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/cmk`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function registerCmkKey(
  tenantId: string,
  publicKeyPem: string,
  algorithm = "RSA-OAEP-256",
): Promise<CmkKey> {
  return requestJSON<CmkKey>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/cmk`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        public_key_pem: publicKeyPem,
        algorithm,
      }),
    },
  );
}

export async function rotateCmkKey(
  tenantId: string,
  keyId: string,
  publicKeyPem: string,
  algorithm = "RSA-OAEP-256",
): Promise<CmkKey> {
  return requestJSON<CmkKey>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/cmk/${encodeURIComponent(keyId)}/rotate`,
    {
      method: "PUT",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        public_key_pem: publicKeyPem,
        algorithm,
      }),
    },
  );
}

export async function revokeCmkKey(
  tenantId: string,
  keyId: string,
): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/cmk/${encodeURIComponent(keyId)}/revoke`,
    { method: "DELETE", headers: adminAuthHeaders(tenantId) },
  );
}

export async function getActiveCmkKey(
  tenantId: string,
): Promise<CmkKey | null> {
  const out = await requestJSON<CmkKey | { key: null }>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/cmk/active`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
  if (out && "key" in out && out.key === null) return null;
  return out as CmkKey;
}

// =====================================================================
// Phase 5 — Protected folders
// =====================================================================

export interface ProtectedFolder {
  id: string;
  tenant_id: string;
  owner_id: string;
  folder_name: string;
  created_at: string;
  updated_at: string;
}

export interface FolderAccess {
  id: string;
  tenant_id: string;
  folder_id: string;
  grantee_id: string;
  permission: "read" | "read_write";
  granted_at: string;
}

export interface FolderAccessLogEntry {
  id: string;
  tenant_id: string;
  folder_id: string;
  actor_id: string;
  action: string;
  created_at: string;
}

export async function listProtectedFolders(
  tenantId: string,
  ownerId?: string,
): Promise<ProtectedFolder[]> {
  const url = ownerId
    ? `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders?owner_id=${encodeURIComponent(ownerId)}`
    : `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders`;
  return requestJSON<ProtectedFolder[]>(url, {
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

export async function createProtectedFolder(
  tenantId: string,
  ownerId: string,
  folderName: string,
): Promise<ProtectedFolder> {
  return requestJSON<ProtectedFolder>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ owner_id: ownerId, folder_name: folderName }),
    },
  );
}

export async function shareProtectedFolder(
  tenantId: string,
  folderId: string,
  ownerId: string,
  granteeId: string,
  permission: FolderAccess["permission"],
): Promise<FolderAccess> {
  return requestJSON<FolderAccess>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders/${encodeURIComponent(folderId)}/share`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({
        owner_id: ownerId,
        grantee_id: granteeId,
        permission,
      }),
    },
  );
}

export async function unshareProtectedFolder(
  tenantId: string,
  folderId: string,
  ownerId: string,
  granteeId: string,
): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders/${encodeURIComponent(folderId)}/unshare`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        Accept: "application/json",
        "Content-Type": "application/json",
      }),
      body: JSON.stringify({ owner_id: ownerId, grantee_id: granteeId }),
    },
  );
}

export async function listProtectedFolderAccess(
  tenantId: string,
  folderId: string,
): Promise<FolderAccess[]> {
  return requestJSON<FolderAccess[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders/${encodeURIComponent(folderId)}/access`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function getProtectedFolderAccessLog(
  tenantId: string,
  folderId: string,
): Promise<FolderAccessLogEntry[]> {
  return requestJSON<FolderAccessLogEntry[]>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/protected-folders/${encodeURIComponent(folderId)}/access-log`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

// =====================================================================
// Phase 5 — Multi-region SLO rollup
// =====================================================================

export interface RegionAvailability {
  region: string;
  total: number;
  successes: number;
  failures: number;
  availability: number;
  target: number;
}

export interface MultiRegionResult {
  window_seconds: number;
  target: number;
  regions: RegionAvailability[];
  global_total: number;
  global_success: number;
  global_failures: number;
  global_availability: number;
}

export async function getSloRegions(): Promise<MultiRegionResult> {
  return requestJSON<MultiRegionResult>(
    `${ADMIN_API_BASE}/admin/slo/regions`,
    { headers: adminAuthHeaders(undefined, { Accept: "application/json" }) },
  );
}
