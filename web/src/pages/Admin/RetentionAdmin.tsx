/**
 * RetentionAdmin manages per-tenant retention policies (auto-archive
 * or auto-delete after N days). Backed by `internal/retention`.
 */

import { useCallback, useEffect, useState } from "react";

import {
  createRetentionPolicy,
  deleteRetentionPolicy,
  getRetentionStatus,
  listRetentionPolicies,
  type RetentionPolicy,
  type RetentionStatus,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function RetentionAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [policies, setPolicies] = useState<RetentionPolicy[]>([]);
  const [status, setStatus] = useState<RetentionStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Partial<RetentionPolicy>>({
    policy_type: "archive",
    retention_days: 90,
    applies_to: "all",
    enabled: true,
  });

  const reload = useCallback((tid: string) => {
    listRetentionPolicies(tid).then(setPolicies).catch((e: unknown) => setError(String(e)));
    getRetentionStatus(tid).then(setStatus).catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId) return;
    try {
      await createRetentionPolicy(selectedTenantId, draft as RetentionPolicy);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDelete = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await deleteRetentionPolicy(selectedTenantId, id);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Retention policies</h2>
      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select value={selectedTenantId ?? ""} onChange={(e) => selectTenant(e.target.value)}>
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </label>
      </div>

      {error && <p className="kmail-error">{error}</p>}

      {selectedTenantId && status && (
        <div
          className="kmail-admin-card"
          style={{
            borderLeft: `4px solid ${status.dry_run ? "#d97706" : "#16a34a"}`,
          }}
        >
          <h3>Enforcement status</h3>
          <p>
            <strong>Mode:</strong>{" "}
            <span style={{ color: status.dry_run ? "#d97706" : "#16a34a" }}>
              {status.dry_run ? "DRY RUN (no rows mutated)" : "LIVE (deleting / archiving)"}
            </span>
          </p>
          <p>
            <strong>Last evaluated:</strong>{" "}
            {status.last_evaluated_at
              ? new Date(status.last_evaluated_at).toLocaleString()
              : "never"}
          </p>
          <p>
            <strong>Cumulative since boot:</strong>{" "}
            {status.emails_deleted} deleted · {status.emails_archived} archived ·{" "}
            {status.errors} errors
          </p>
          {status.dry_run && (
            <p style={{ fontSize: 13, color: "#92400e" }}>
              The worker is in dry-run mode (<code>KMAIL_RETENTION_DRY_RUN=true</code>).
              Live enforcement is the Phase 6 default — set the env var to <code>false</code>{" "}
              or unset it to switch on.
            </p>
          )}
        </div>
      )}

      {selectedTenantId && (
        <>
          <form onSubmit={onCreate} style={{ display: "grid", gap: "0.5rem", maxWidth: 480 }}>
            <h3>New policy</h3>
            <label>
              Action
              <select
                value={draft.policy_type ?? "archive"}
                onChange={(e) => setDraft({ ...draft, policy_type: e.target.value as RetentionPolicy["policy_type"] })}
              >
                <option value="archive">Archive (cold-tier)</option>
                <option value="delete">Delete</option>
              </select>
            </label>
            <label>
              After (days)
              <input
                type="number"
                min={1}
                value={draft.retention_days ?? 90}
                onChange={(e) => setDraft({ ...draft, retention_days: Number(e.target.value) })}
              />
            </label>
            <label>
              Applies to
              <select
                value={draft.applies_to ?? "all"}
                onChange={(e) => setDraft({ ...draft, applies_to: e.target.value as RetentionPolicy["applies_to"] })}
              >
                <option value="all">All mail</option>
                <option value="mailbox">Mailbox</option>
                <option value="label">Label</option>
              </select>
            </label>
            {(draft.applies_to === "mailbox" || draft.applies_to === "label") && (
              <label>
                Target ref
                <input
                  value={draft.target_ref ?? ""}
                  onChange={(e) => setDraft({ ...draft, target_ref: e.target.value })}
                />
              </label>
            )}
            <button type="submit">Add policy</button>
          </form>

          <h3 style={{ marginTop: "1.5rem" }}>Existing policies</h3>
          <table>
            <thead>
              <tr>
                <th>Type</th><th>Days</th><th>Applies to</th><th>Target</th><th>Enabled</th><th />
              </tr>
            </thead>
            <tbody>
              {policies.map((p) => (
                <tr key={p.id}>
                  <td>{p.policy_type}</td>
                  <td>{p.retention_days}</td>
                  <td>{p.applies_to}</td>
                  <td>{p.target_ref ?? ""}</td>
                  <td>{p.enabled ? "yes" : "no"}</td>
                  <td><button type="button" onClick={() => onDelete(p.id)}>Delete</button></td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}
