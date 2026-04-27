import { useCallback, useEffect, useState } from "react";

import {
  AdminApiError,
  changePlan,
  getBillingSummary,
  openBillingPortal,
  PLAN_CATALOG,
  type BillingPlan,
  type BillingSummary,
  type PlanCatalogEntry,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * PricingAdmin renders the public KMail pricing matrix, highlights
 * the tenant's current plan, and lets an admin upgrade or downgrade
 * via `PATCH /api/v1/tenants/{id}/billing/plan`. The plan catalog
 * itself lives in `api/admin.ts` so the same source of truth drives
 * marketing copy and the upgrade flow.
 */
export default function PricingAdmin() {
  const {
    tenants,
    selectedTenantId,
    selectedTenant,
    selectTenant,
  } = useTenantSelection();

  const [summary, setSummary] = useState<BillingSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [pendingPlan, setPendingPlan] = useState<BillingPlan | null>(null);

  const reload = useCallback((tenantId: string) => {
    let cancelled = false;
    getBillingSummary(tenantId)
      .then((s) => {
        if (cancelled) return;
        setSummary(s);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setError(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!selectedTenantId) return;
    return reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onChangePlan = async (plan: BillingPlan) => {
    if (!selectedTenantId || pendingPlan) return;
    setError(null);
    setInfo(null);
    setPendingPlan(plan);
    try {
      const next = await changePlan(selectedTenantId, plan);
      setSummary(next);
      setInfo(`Plan changed to ${planLabel(plan)}.`);
    } catch (e) {
      // ChangePlan commits the new `tenants.plan` row first and
      // only then runs EnforcePlanLimits, so a 402 means the
      // database is on the new plan but is over its quota.
      // Refresh the summary so the UI matches reality and surface
      // the quota warning as info rather than a hard failure.
      if (e instanceof AdminApiError && e.status === 402) {
        reload(selectedTenantId);
        setInfo(
          `Plan changed to ${planLabel(plan)}, but the tenant is over the new plan's quota — adjust seats or storage.`,
        );
      } else {
        setError(errorMessage(e));
      }
    } finally {
      setPendingPlan(null);
    }
  };

  const currentPlan = (summary?.plan ?? selectedTenant?.plan ?? "core") as BillingPlan;
  const seatCount = summary?.seat_count ?? 0;

  return (
    <section className="kmail-admin-page kmail-pricing-admin">
      <h2>Pricing &amp; plan management</h2>

      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select
            value={selectedTenantId ?? ""}
            onChange={(e) => selectTenant(e.target.value)}
          >
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

      {selectedTenantId && (
        <p className="kmail-actions">
          <button
            type="button"
            onClick={async () => {
              if (!selectedTenantId) return;
              try {
                const session = await openBillingPortal(
                  selectedTenantId,
                  window.location.href,
                );
                window.location.href = session.url;
              } catch (e: unknown) {
                if (e instanceof AdminApiError && e.status === 503) {
                  setInfo("Billing portal not configured (KMAIL_STRIPE_SECRET_KEY missing).");
                } else {
                  setError(String(e));
                }
              }
            }}
          >
            Manage subscription
          </button>
        </p>
      )}

      {summary && (
        <p className="kmail-pricing-summary">
          <strong>{seatCount}</strong> active seat{seatCount === 1 ? "" : "s"} ·
          current monthly total{" "}
          <strong>${formatCents(seatCount * planPriceCents(currentPlan))}</strong>
        </p>
      )}

      <div
        className="kmail-pricing-grid"
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(3, minmax(0, 1fr))",
          gap: "1rem",
        }}
      >
        {PLAN_CATALOG.map((plan) => (
          <PlanCard
            key={plan.id}
            plan={plan}
            seatCount={seatCount}
            isCurrent={plan.id === currentPlan}
            onSelect={() => onChangePlan(plan.id)}
            disabled={!selectedTenantId || pendingPlan !== null}
            pending={pendingPlan === plan.id}
          />
        ))}
      </div>
    </section>
  );
}

interface PlanCardProps {
  plan: PlanCatalogEntry;
  seatCount: number;
  isCurrent: boolean;
  onSelect: () => void;
  disabled: boolean;
  pending: boolean;
}

function PlanCard({
  plan,
  seatCount,
  isCurrent,
  onSelect,
  disabled,
  pending,
}: PlanCardProps) {
  const monthly = seatCount * plan.monthlyPriceCents;
  return (
    <div
      className={`kmail-plan-card${isCurrent ? " kmail-plan-card-current" : ""}`}
      style={{
        border: isCurrent ? "2px solid var(--kmail-accent, #2563eb)" : "1px solid #d1d5db",
        borderRadius: "0.5rem",
        padding: "1rem",
        background: isCurrent ? "rgba(37, 99, 235, 0.05)" : undefined,
      }}
    >
      <header>
        <h3>{plan.name}</h3>
        <p className="kmail-plan-card-price">
          <strong>${formatCents(plan.monthlyPriceCents)}</strong> / seat / mo
        </p>
        {isCurrent && <span className="kmail-plan-card-badge">Current plan</span>}
      </header>
      <ul className="kmail-plan-card-specs">
        <li>{plan.dailySendLimit.toLocaleString()} sends / day</li>
        <li>{plan.storagePerSeatGB} GB storage / seat</li>
      </ul>
      <ul className="kmail-plan-card-features">
        {plan.features.map((f) => (
          <li key={f}>{f}</li>
        ))}
      </ul>
      <p className="kmail-plan-card-monthly">
        Estimated monthly: ${formatCents(monthly)} ({seatCount} seats)
      </p>
      <button
        type="button"
        onClick={onSelect}
        disabled={disabled || isCurrent}
        className="kmail-plan-card-cta"
      >
        {isCurrent
          ? "Current plan"
          : pending
            ? "Updating…"
            : `Switch to ${plan.name}`}
      </button>
    </div>
  );
}

function planLabel(plan: BillingPlan): string {
  return PLAN_CATALOG.find((p) => p.id === plan)?.name ?? plan;
}

function planPriceCents(plan: BillingPlan): number {
  return PLAN_CATALOG.find((p) => p.id === plan)?.monthlyPriceCents ?? 0;
}

function formatCents(cents: number): string {
  return (cents / 100).toFixed(2);
}

function errorMessage(e: unknown): string {
  if (e instanceof AdminApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}
