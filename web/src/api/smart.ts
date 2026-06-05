/**
 * Typed REST client for the WS7 "Smart Features & Intelligence"
 * surface served by `internal/smartfeatures` and `internal/priority`:
 *
 *   GET  /api/v1/priority-inbox
 *   GET  /api/v1/emails/{id}/smart-replies
 *   GET  /api/v1/emails/{id}/unsubscribe
 *   POST /api/v1/emails/{id}/unsubscribe
 *   POST /api/v1/emails/categories
 *   GET  /api/v1/contacts/frequent
 *   GET  /api/v1/contacts/suggestions
 *   POST /api/v1/contacts/record
 *   GET  /api/v1/email-analytics
 *
 * Auth mirrors `jmap.ts` / `admin.ts`: the dev-bypass bearer token
 * is sent on every request and the Go OIDC middleware resolves the
 * acting tenant + user from it (or from a real KChat token in
 * staging / production). The optional tenant id drives the
 * `X-KMail-Dev-Tenant-Id` dev-bypass header so the admin analytics
 * page can scope to the selected tenant in the local stack.
 */
import { DEV_BEARER_TOKEN } from "./jmap";

const SMART_API_BASE = "/api/v1";

function smartHeaders(tenantId?: string, extra: HeadersInit = {}): Headers {
  const h = new Headers(extra);
  h.set("Authorization", `Bearer ${DEV_BEARER_TOKEN}`);
  if (tenantId) {
    h.set("X-KMail-Dev-Tenant-Id", tenantId);
  }
  return h;
}

/** Thrown for any non-2xx response from the smart-features API. */
export class SmartApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(`${status} ${message}`);
    this.name = "SmartApiError";
    this.status = status;
  }
}

async function getJSON<T>(url: string, tenantId?: string): Promise<T> {
  const res = await fetch(url, {
    method: "GET",
    credentials: "include",
    headers: smartHeaders(tenantId, { Accept: "application/json" }),
  });
  if (!res.ok) {
    throw new SmartApiError(res.status, await errorText(res));
  }
  return (await res.json()) as T;
}

async function postJSON<T>(url: string, body: unknown, tenantId?: string): Promise<T> {
  const res = await fetch(url, {
    method: "POST",
    credentials: "include",
    headers: smartHeaders(tenantId, {
      Accept: "application/json",
      "Content-Type": "application/json",
    }),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    throw new SmartApiError(res.status, await errorText(res));
  }
  return (await res.json()) as T;
}

async function errorText(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: string };
    if (body && typeof body.error === "string") return body.error;
  } catch {
    // non-JSON body — fall through to status text
  }
  return res.statusText;
}

// ─── Priority inbox ──────────────────────────────────────────────

/**
 * An RFC 8621 mailbox address. Mirrors `smartfeatures.Address`
 * (`{ name?, email }`) — the Go handlers serialize sender/recipient
 * lists as arrays of these objects, never as pre-formatted strings.
 */
export interface EmailAddress {
  name?: string;
  email: string;
}

/** Format an address list as `Name <email>, …` for display. */
export function formatAddresses(addrs?: EmailAddress[]): string {
  if (!addrs || addrs.length === 0) return "";
  return addrs
    .map((a) => (a.name ? `${a.name} <${a.email}>` : a.email))
    .join(", ");
}

/** One ranked message in the priority inbox. */
export interface PriorityItem {
  email_id: string;
  thread_id: string;
  score: number;
  subject: string;
  preview: string;
  from?: EmailAddress[];
  received_at: string;
}

export interface PriorityInboxResponse {
  cached: boolean;
  items: PriorityItem[];
}

/**
 * Fetch the priority inbox. `cached` serves the last computed
 * ranking from Valkey without a Stalwart round-trip; omit it to
 * force a recompute (which also refreshes the cache).
 */
export async function getPriorityInbox(
  opts: { limit?: number; cached?: boolean } = {},
): Promise<PriorityInboxResponse> {
  const params = new URLSearchParams();
  if (opts.limit) params.set("limit", String(opts.limit));
  if (opts.cached) params.set("cached", "1");
  const qs = params.toString();
  return getJSON<PriorityInboxResponse>(
    `${SMART_API_BASE}/priority-inbox${qs ? `?${qs}` : ""}`,
  );
}

// ─── Smart replies ───────────────────────────────────────────────

export interface SmartReplySuggestion {
  text: string;
  /** Coarse intent bucket: "affirm" | "decline" | "defer" | "ack". */
  tone: string;
}

export interface SmartRepliesResponse {
  email_id: string;
  suggestions: SmartReplySuggestion[];
}

export async function getSmartReplies(emailId: string): Promise<SmartRepliesResponse> {
  return getJSON<SmartRepliesResponse>(
    `${SMART_API_BASE}/emails/${encodeURIComponent(emailId)}/smart-replies`,
  );
}

// ─── Categorization ──────────────────────────────────────────────

/** Gmail-style categories returned by the categorizer. */
export type EmailCategory =
  | "primary"
  | "social"
  | "promotions"
  | "updates"
  | "forums";

export interface CategoriesResponse {
  categories: Record<string, EmailCategory>;
}

export async function categorize(ids: string[]): Promise<CategoriesResponse> {
  if (ids.length === 0) return { categories: {} };
  return postJSON<CategoriesResponse>(`${SMART_API_BASE}/emails/categories`, { ids });
}

// ─── Unsubscribe ─────────────────────────────────────────────────

export interface UnsubscribeInfoResponse {
  email_id: string;
  unsubscribe: boolean;
  already_done: boolean;
  one_click: boolean;
  http?: string;
  mailto?: string;
  list_id?: string;
}

export async function getUnsubscribe(emailId: string): Promise<UnsubscribeInfoResponse> {
  return getJSON<UnsubscribeInfoResponse>(
    `${SMART_API_BASE}/emails/${encodeURIComponent(emailId)}/unsubscribe`,
  );
}

export interface UnsubscribeResultResponse {
  email_id: string;
  /** "one-click" | "recorded". */
  method: string;
  mailto?: string;
}

export async function postUnsubscribe(emailId: string): Promise<UnsubscribeResultResponse> {
  return postJSON<UnsubscribeResultResponse>(
    `${SMART_API_BASE}/emails/${encodeURIComponent(emailId)}/unsubscribe`,
    undefined,
  );
}

// ─── Frequent contacts ───────────────────────────────────────────

export interface FrequentContact {
  email: string;
  name?: string;
  count: number;
}

export interface FrequentContactsResponse {
  contacts: FrequentContact[];
}

export async function getFrequentContacts(limit = 10): Promise<FrequentContactsResponse> {
  return getJSON<FrequentContactsResponse>(
    `${SMART_API_BASE}/contacts/frequent?limit=${encodeURIComponent(String(limit))}`,
  );
}

export interface CoRecipientSuggestion {
  email: string;
  name?: string;
  count: number;
}

export interface CoRecipientsResponse {
  anchor: string;
  suggestions: CoRecipientSuggestion[];
}

export async function getCoRecipients(
  anchor: string,
  exclude: string[] = [],
): Promise<CoRecipientsResponse> {
  const params = new URLSearchParams({ anchor });
  for (const e of exclude) params.append("exclude", e);
  return getJSON<CoRecipientsResponse>(
    `${SMART_API_BASE}/contacts/suggestions?${params.toString()}`,
  );
}

export async function recordSend(recipients: string[]): Promise<void> {
  await postJSON<unknown>(`${SMART_API_BASE}/contacts/record`, { recipients });
}

// ─── Email analytics ─────────────────────────────────────────────

export interface DailyCount {
  date: string;
  sent: number;
  received: number;
}

export interface NamedCount {
  email: string;
  name?: string;
  count: number;
}

export interface HourCount {
  hour: number;
  count: number;
}

export interface EmailAnalytics {
  range_start: string;
  range_end: string;
  total_sent: number;
  total_received: number;
  daily: DailyCount[];
  top_recipients: NamedCount[];
  top_senders: NamedCount[];
  busiest_hours: HourCount[];
  avg_response_seconds: number;
  response_sample_size: number;
}

export async function getEmailAnalytics(
  opts: { days?: number; tz?: string; tenantId?: string } = {},
): Promise<EmailAnalytics> {
  const params = new URLSearchParams();
  if (opts.days) params.set("days", String(opts.days));
  if (opts.tz) params.set("tz", opts.tz);
  const qs = params.toString();
  return getJSON<EmailAnalytics>(
    `${SMART_API_BASE}/email-analytics${qs ? `?${qs}` : ""}`,
    opts.tenantId,
  );
}
