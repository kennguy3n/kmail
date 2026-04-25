/**
 * Billing client wrappers. Phase 4 separates the published-pricing
 * surface (current plan, plan change, proration preview, billing
 * history) from the broader admin-area client in `api/admin.ts` so
 * the marketing-driven `/admin/pricing` page can be vendored into a
 * future public landing site without dragging the rest of the
 * admin client surface along.
 */

import {
  AdminApiError,
  PLAN_CATALOG,
  changePlan as adminChangePlan,
  getBillingSummary,
  type BillingPlan,
  type BillingSummary,
  type PlanCatalogEntry,
} from "./admin";

export type { BillingPlan, BillingSummary, PlanCatalogEntry };
export { AdminApiError, PLAN_CATALOG };

const BASE = "/api/v1";

function authHeaders(tenantId: string): HeadersInit {
  // Mirror the admin client: the BFF accepts `X-Kmail-Tenant` as a
  // dev-bypass shortcut; production deployments rely on the OIDC
  // bearer token attached upstream.
  return { "X-Kmail-Tenant": tenantId };
}

export async function getCurrentPlan(tenantId: string): Promise<BillingSummary> {
  return getBillingSummary(tenantId);
}

export async function changePlan(
  tenantId: string,
  plan: BillingPlan,
): Promise<BillingSummary> {
  return adminChangePlan(tenantId, plan);
}

/** ProrationPreview is the response shape of GET .../proration-preview. */
export interface ProrationPreview {
  tenant_id: string;
  new_plan: BillingPlan;
  proration_cents: number;
}

export async function getProrationPreview(
  tenantId: string,
  plan: BillingPlan,
): Promise<ProrationPreview> {
  const url = `${BASE}/tenants/${encodeURIComponent(tenantId)}/billing/proration-preview?plan=${encodeURIComponent(plan)}`;
  const res = await fetch(url, { headers: authHeaders(tenantId) });
  if (!res.ok) {
    const text = await res.text();
    throw new AdminApiError(url, res.status, `proration preview: ${res.status} ${text}`);
  }
  return (await res.json()) as ProrationPreview;
}

/** BillingHistoryEntry mirrors `internal/billing/lifecycle.go`. */
export interface BillingHistoryEntry {
  id: string;
  event_type: string;
  amount_cents: number;
  seat_count: number;
  metadata: string;
  created_at: string;
}

export async function getBillingHistory(
  tenantId: string,
): Promise<BillingHistoryEntry[]> {
  const url = `${BASE}/tenants/${encodeURIComponent(tenantId)}/billing/history`;
  const res = await fetch(url, { headers: authHeaders(tenantId) });
  if (!res.ok) {
    const text = await res.text();
    throw new AdminApiError(url, res.status, `billing history: ${res.status} ${text}`);
  }
  return (await res.json()) as BillingHistoryEntry[];
}
