import { useCallback, useEffect, useState } from "react";

import {
  AdminApiError,
  getBillingSummary,
  getQuota,
  updateQuotaLimits,
  type BillingSummary,
  type Quota,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * QuotaAdmin surfaces per-tenant billing + quota state.
 *
 * Shows storage usage vs. the pooled limit, seat count vs. the
 * seat limit, per-seat pricing for the tenant's plan, and an
 * estimated monthly total. Admins can adjust `storage_limit_bytes`
 * and `seat_limit` via `PATCH /api/v1/tenants/{id}/billing`.
 */
export default function QuotaAdmin() {
  const {
    tenants,
    selectedTenantId,
    selectedTenant,
    selectTenant,
    isLoading: tenantsLoading,
    error: tenantsError,
  } = useTenantSelection();

  const [summary, setSummary] = useState<BillingSummary | null>(null);
  const [quota, setQuota] = useState<Quota | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<{ storage_limit_gb: string; seat_limit: string }>({
    storage_limit_gb: "",
    seat_limit: "",
  });
  const [saving, setSaving] = useState(false);

  const load = useCallback((tenantId: string) => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    Promise.all([getBillingSummary(tenantId), getQuota(tenantId)])
      .then(([s, q]) => {
        if (cancelled) return;
        setSummary(s);
        setQuota(q);
        setDraft({
          storage_limit_gb: String(Math.round((q.storage_limit_bytes ?? 0) / (1024 * 1024 * 1024))),
          seat_limit: String(q.seat_limit ?? 0),
        });
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setError(errorMessage(e));
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!selectedTenantId) return;
    return load(selectedTenantId);
  }, [selectedTenantId, load]);

  const save = (): void => {
    if (!selectedTenantId) return;
    setSaving(true);
    setError(null);
    const patch: { storage_limit_bytes?: number; seat_limit?: number } = {};
    const gb = Number(draft.storage_limit_gb);
    if (!Number.isNaN(gb) && gb > 0) {
      patch.storage_limit_bytes = Math.floor(gb * 1024 * 1024 * 1024);
    }
    const seats = Number(draft.seat_limit);
    if (!Number.isNaN(seats) && seats > 0) {
      patch.seat_limit = Math.floor(seats);
    }
    updateQuotaLimits(selectedTenantId, patch)
      .then((q) => {
        setQuota(q);
        if (selectedTenantId) load(selectedTenantId);
      })
      .catch((e: unknown) => setError(errorMessage(e)))
      .finally(() => setSaving(false));
  };

  return (
    <section>
      <h2>Billing &amp; quota</h2>

      <label>
        Tenant:&nbsp;
        <select
          value={selectedTenantId ?? ""}
          onChange={(e) => selectTenant(e.target.value)}
          disabled={tenantsLoading || !tenants?.length}
        >
          {tenants?.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name} ({t.plan})
            </option>
          ))}
        </select>
      </label>

      {tenantsError && <p role="alert">Tenants: {tenantsError}</p>}
      {error && <p role="alert">{error}</p>}
      {loading && <p>Loading…</p>}

      {summary && quota && (
        <>
          <h3>Plan: {summary.plan}</h3>
          <p>
            Per-seat price: {formatUsd(summary.per_seat_cents)} / month
            &nbsp;·&nbsp; Estimated monthly total:{" "}
            <strong>{formatUsd(summary.monthly_total_cents)}</strong>
          </p>

          <h3>Storage</h3>
          <UsageBar used={quota.storage_used_bytes} limit={quota.storage_limit_bytes} />
          <p>
            {formatBytes(quota.storage_used_bytes)} of {formatBytes(quota.storage_limit_bytes)} used
          </p>

          <h3>Seats</h3>
          <UsageBar used={quota.seat_count} limit={quota.seat_limit} />
          <p>
            {quota.seat_count} of {quota.seat_limit} seats used
          </p>

          <h3>Adjust limits</h3>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              save();
            }}
          >
            <label>
              Storage limit (GB):&nbsp;
              <input
                type="number"
                min={1}
                value={draft.storage_limit_gb}
                onChange={(e) => setDraft((d) => ({ ...d, storage_limit_gb: e.target.value }))}
              />
            </label>
            &nbsp;
            <label>
              Seat limit:&nbsp;
              <input
                type="number"
                min={1}
                value={draft.seat_limit}
                onChange={(e) => setDraft((d) => ({ ...d, seat_limit: e.target.value }))}
              />
            </label>
            &nbsp;
            <button type="submit" disabled={saving}>
              {saving ? "Saving…" : "Save limits"}
            </button>
          </form>
          {selectedTenant && (
            <p>
              <small>Tenant ID: {selectedTenant.id}</small>
            </p>
          )}
        </>
      )}
    </section>
  );
}

function UsageBar({ used, limit }: { used: number; limit: number }) {
  const pct = limit > 0 ? Math.min(100, Math.round((used / limit) * 100)) : 0;
  const color = pct >= 90 ? "#c0392b" : pct >= 75 ? "#e67e22" : "#2980b9";
  return (
    <div
      style={{
        background: "#eee",
        height: 14,
        borderRadius: 4,
        overflow: "hidden",
        maxWidth: 480,
      }}
    >
      <div
        aria-valuenow={pct}
        aria-valuemin={0}
        aria-valuemax={100}
        role="progressbar"
        style={{
          width: `${pct}%`,
          height: "100%",
          background: color,
          transition: "width 150ms linear",
        }}
      />
    </div>
  );
}

function formatBytes(n: number): string {
  if (!n || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

function formatUsd(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

function errorMessage(e: unknown): string {
  if (e instanceof AdminApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}
