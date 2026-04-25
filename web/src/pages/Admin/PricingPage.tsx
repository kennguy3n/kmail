/**
 * PricingPage is the public-facing pricing comparison + plan
 * selection surface. It renders the three-tier KChat pricing matrix
 * (Core Email, Mail Pro, Privacy), highlights the tenant's current
 * plan, and gates plan changes behind a confirmation modal that
 * shows the prorated charge for the remaining billing period.
 *
 * The legacy `/admin/pricing` route (PricingAdmin) stays put so
 * deep links keep working; this page powers a richer feature-matrix
 * comparison and is mounted at `/admin/pricing-plans`.
 */

import { useCallback, useEffect, useState } from "react";

import {
  AdminApiError,
  changePlan,
  getCurrentPlan,
  getProrationPreview,
  PLAN_CATALOG,
  type BillingPlan,
  type BillingSummary,
  type PlanCatalogEntry,
  type ProrationPreview,
} from "../../api/billing";
import { useTenantSelection } from "./useTenantSelection";

const FEATURE_MATRIX: Array<{
  label: string;
  values: Record<BillingPlan, string>;
}> = [
  {
    label: "Storage / seat",
    values: { core: "5 GB", pro: "15 GB", privacy: "50 GB" },
  },
  {
    label: "Daily send limit",
    values: { core: "500", pro: "2,000", privacy: "5,000" },
  },
  {
    label: "Search tier",
    values: { core: "Core", pro: "Pro", privacy: "Pro" },
  },
  {
    label: "Shared inboxes",
    values: { core: "Yes", pro: "Yes", privacy: "Yes" },
  },
  { label: "Calendar / CalDAV", values: { core: "Yes", pro: "Yes", privacy: "Yes" } },
  { label: "Spam filtering", values: { core: "Yes", pro: "Yes", privacy: "Yes" } },
  {
    label: "Confidential Send (StrictZK)",
    values: { core: "—", pro: "—", privacy: "Yes" },
  },
  {
    label: "Zero-Access Vault",
    values: { core: "—", pro: "—", privacy: "Yes" },
  },
  {
    label: "Custom retention policies",
    values: { core: "—", pro: "Yes", privacy: "Yes" },
  },
  {
    label: "Priority support",
    values: { core: "—", pro: "Yes", privacy: "Yes" },
  },
];

export default function PricingPage() {
  const { tenants, selectedTenantId, selectedTenant, selectTenant } = useTenantSelection();
  const [summary, setSummary] = useState<BillingSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [pendingPlan, setPendingPlan] = useState<BillingPlan | null>(null);
  const [proration, setProration] = useState<ProrationPreview | null>(null);
  const [committing, setCommitting] = useState(false);

  const reload = useCallback((tenantId: string) => {
    let cancelled = false;
    getCurrentPlan(tenantId)
      .then((s) => {
        if (cancelled) return;
        setSummary(s);
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!selectedTenantId) return;
    return reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const startPlanChange = async (plan: BillingPlan) => {
    if (!selectedTenantId) return;
    setError(null);
    setInfo(null);
    setProration(null);
    setPendingPlan(plan);
    try {
      const preview = await getProrationPreview(selectedTenantId, plan);
      setProration(preview);
    } catch (e) {
      setError(errorMessage(e));
      setPendingPlan(null);
    }
  };

  const cancelPlanChange = () => {
    setPendingPlan(null);
    setProration(null);
  };

  const confirmPlanChange = async () => {
    if (!selectedTenantId || !pendingPlan) return;
    setCommitting(true);
    try {
      const next = await changePlan(selectedTenantId, pendingPlan);
      setSummary(next);
      setInfo(`Plan changed to ${planLabel(pendingPlan)}.`);
      setPendingPlan(null);
      setProration(null);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setCommitting(false);
    }
  };

  const currentPlan = (summary?.plan ?? selectedTenant?.plan ?? "core") as BillingPlan;
  const seatCount = summary?.seat_count ?? 0;

  return (
    <section className="kmail-admin-page kmail-pricing-page">
      <h2>KChat Mail pricing</h2>
      <p className="kmail-pricing-blurb">
        Pick the tier that matches your team. All plans include calendar, shared
        inboxes, and DKIM/SPF/DMARC onboarding.
      </p>

      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select value={selectedTenantId ?? ""} onChange={(e) => selectTenant(e.target.value)}>
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
        </label>
      </div>

      {error && <p className="kmail-error">{error}</p>}
      {info && <p className="kmail-info">{info}</p>}

      <div
        className="kmail-pricing-grid"
        style={{ display: "grid", gridTemplateColumns: "repeat(3, minmax(0, 1fr))", gap: "1rem" }}
      >
        {PLAN_CATALOG.map((plan) => (
          <PlanCard
            key={plan.id}
            plan={plan}
            seatCount={seatCount}
            isCurrent={plan.id === currentPlan}
            disabled={!selectedTenantId || pendingPlan !== null}
            onSelect={() => startPlanChange(plan.id)}
          />
        ))}
      </div>

      <h3 style={{ marginTop: "2rem" }}>Feature comparison</h3>
      <table className="kmail-pricing-matrix">
        <thead>
          <tr>
            <th />
            {PLAN_CATALOG.map((p) => (
              <th key={p.id}>{p.name}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {FEATURE_MATRIX.map((row) => (
            <tr key={row.label}>
              <th scope="row">{row.label}</th>
              {PLAN_CATALOG.map((p) => (
                <td key={p.id}>{row.values[p.id]}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>

      {pendingPlan && proration && (
        <ProrationModal
          plan={pendingPlan}
          proration={proration}
          committing={committing}
          onCancel={cancelPlanChange}
          onConfirm={confirmPlanChange}
        />
      )}
    </section>
  );
}

interface PlanCardProps {
  plan: PlanCatalogEntry;
  seatCount: number;
  isCurrent: boolean;
  disabled: boolean;
  onSelect: () => void;
}

function PlanCard({ plan, seatCount, isCurrent, disabled, onSelect }: PlanCardProps) {
  const monthly = seatCount * plan.monthlyPriceCents;
  return (
    <div
      className={`kmail-plan-card${isCurrent ? " kmail-plan-card-current" : ""}`}
      style={{
        border: isCurrent ? "2px solid var(--kmail-accent, #2563eb)" : "1px solid #d1d5db",
        borderRadius: "0.5rem",
        padding: "1rem",
      }}
    >
      <h3>{plan.name}</h3>
      <p>
        <strong>${formatCents(plan.monthlyPriceCents)}</strong> / seat / mo
      </p>
      {isCurrent && <span className="kmail-plan-card-badge">Current plan</span>}
      <ul>
        <li>{plan.dailySendLimit.toLocaleString()} sends / day</li>
        <li>{plan.storagePerSeatGB} GB storage / seat</li>
      </ul>
      <p>
        Estimated monthly: ${formatCents(monthly)} ({seatCount} seats)
      </p>
      <button type="button" disabled={disabled || isCurrent} onClick={onSelect}>
        {isCurrent ? "Current plan" : `Switch to ${plan.name}`}
      </button>
    </div>
  );
}

interface ProrationModalProps {
  plan: BillingPlan;
  proration: ProrationPreview;
  committing: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}

function ProrationModal({ plan, proration, committing, onCancel, onConfirm }: ProrationModalProps) {
  const cents = proration.proration_cents;
  const positive = cents >= 0;
  return (
    <div
      className="kmail-modal-backdrop"
      role="dialog"
      aria-modal="true"
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(0,0,0,0.45)",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
      }}
    >
      <div
        className="kmail-modal"
        style={{ background: "white", padding: "1.5rem", borderRadius: "0.5rem", minWidth: 360 }}
      >
        <h3>Confirm plan change</h3>
        <p>
          Switching to <strong>{planLabel(plan)}</strong>.
        </p>
        <p>
          {positive ? "Prorated charge" : "Prorated credit"} for the remaining billing period:{" "}
          <strong>${formatCents(Math.abs(cents))}</strong>
        </p>
        <p style={{ fontSize: "0.85rem", color: "#6b7280" }}>
          The full new monthly rate kicks in on the next billing cycle. Storage limits
          apply immediately — make sure the tenant is not over the new plan&rsquo;s quota.
        </p>
        <div style={{ display: "flex", gap: "0.5rem", justifyContent: "flex-end" }}>
          <button type="button" onClick={onCancel} disabled={committing}>
            Cancel
          </button>
          <button type="button" onClick={onConfirm} disabled={committing}>
            {committing ? "Updating…" : "Confirm change"}
          </button>
        </div>
      </div>
    </div>
  );
}

function planLabel(plan: BillingPlan): string {
  return PLAN_CATALOG.find((p) => p.id === plan)?.name ?? plan;
}

function formatCents(cents: number): string {
  return (cents / 100).toFixed(2);
}

function errorMessage(e: unknown): string {
  if (e instanceof AdminApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}
