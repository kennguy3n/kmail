import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { getTenant, updateTenant } from "../../api/admin";
import type { Tenant, TenantPatch, TenantPlan } from "../../types";

/**
 * TenantAdmin is the tenant-level admin console (Phase 3).
 *
 * Reads `?tenantId=` from the URL so the page can be bookmarked
 * per tenant; falls back to the dev tenant UUID for local work.
 */
const DEV_TENANT_ID = "00000000-0000-0000-0000-000000000000";
const PLAN_PRICING: Record<TenantPlan, string> = {
  core: "$3 / seat / mo",
  pro: "$6 / seat / mo",
  privacy: "$9 / seat / mo",
};

function resolveTenantId(): string {
  const params = new URLSearchParams(window.location.search);
  return params.get("tenantId") ?? DEV_TENANT_ID;
}

export default function TenantAdmin() {
  const [tenantId] = useState(resolveTenantId);
  const [tenant, setTenant] = useState<Tenant | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [form, setForm] = useState<TenantPatch>({});

  useEffect(() => {
    let cancelled = false;
    getTenant(tenantId)
      .then((t) => {
        if (cancelled) return;
        setTenant(t);
        setForm({ name: t.name, plan: t.plan, status: t.status });
      })
      .catch((err: Error) => !cancelled && setError(err.message));
    return () => {
      cancelled = true;
    };
  }, [tenantId]);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setSaving(true);
    setError(null);
    try {
      const updated = await updateTenant(tenantId, form);
      setTenant(updated);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <section>
      <h2>Tenant admin</h2>
      <p style={{ fontSize: "0.9em", color: "#666" }}>Tenant ID: {tenantId}</p>
      {error && <div style={{ color: "crimson" }}>Error: {error}</div>}
      {!tenant && !error && <p>Loading…</p>}
      {tenant && (
        <form onSubmit={onSubmit} style={{ maxWidth: 480 }}>
          <div style={{ marginBottom: 12 }}>
            <label>
              Name
              <input
                type="text"
                value={form.name ?? ""}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                style={{ display: "block", width: "100%" }}
              />
            </label>
          </div>
          <div style={{ marginBottom: 12 }}>
            <label>
              Slug (read-only)
              <input
                type="text"
                value={tenant.slug}
                readOnly
                style={{ display: "block", width: "100%", background: "#f5f5f5" }}
              />
            </label>
          </div>
          <div style={{ marginBottom: 12 }}>
            <label>
              Plan
              <select
                value={form.plan ?? tenant.plan}
                onChange={(e) =>
                  setForm({ ...form, plan: e.target.value as TenantPlan })
                }
                style={{ display: "block", width: "100%" }}
              >
                <option value="core">Core — {PLAN_PRICING.core}</option>
                <option value="pro">Pro — {PLAN_PRICING.pro}</option>
                <option value="privacy">Privacy — {PLAN_PRICING.privacy}</option>
              </select>
            </label>
          </div>
          <div style={{ marginBottom: 12 }}>
            <label>
              Status
              <select
                value={form.status ?? tenant.status}
                onChange={(e) =>
                  setForm({
                    ...form,
                    status: e.target.value as Tenant["status"],
                  })
                }
                style={{ display: "block", width: "100%" }}
              >
                <option value="active">Active</option>
                <option value="suspended">Suspended</option>
              </select>
            </label>
          </div>
          <button type="submit" disabled={saving}>
            {saving ? "Saving…" : "Save"}
          </button>
          <p style={{ marginTop: 16, fontSize: "0.85em", color: "#666" }}>
            Created {new Date(tenant.createdAt).toLocaleString()} · last
            updated {new Date(tenant.updatedAt).toLocaleString()}
          </p>
        </form>
      )}
    </section>
  );
}
