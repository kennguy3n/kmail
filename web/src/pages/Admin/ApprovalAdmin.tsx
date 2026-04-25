/**
 * ApprovalAdmin lists pending approvals, lets reviewers approve /
 * reject sensitive admin actions, and shows recent approval
 * history. Action gating is configured via the inline checkboxes
 * (PUT /api/v1/tenants/{id}/approvals/config).
 */

import { useCallback, useEffect, useState } from "react";

import {
  approveApprovalRequest,
  getApprovalConfig,
  listApprovals,
  rejectApprovalRequest,
  setApprovalConfig,
  type ApprovalRequest,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

const GATED_ACTIONS = [
  "user_delete",
  "domain_remove",
  "data_export",
  "plan_downgrade",
  "retention_policy_change",
];

export default function ApprovalAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [pending, setPending] = useState<ApprovalRequest[]>([]);
  const [history, setHistory] = useState<ApprovalRequest[]>([]);
  const [config, setConfig] = useState<Record<string, boolean>>({});
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    listApprovals(tid, "pending").then(setPending).catch((e) => setError(String(e)));
    listApprovals(tid).then(setHistory).catch(() => undefined);
    getApprovalConfig(tid).then(setConfig).catch(() => undefined);
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onApprove = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await approveApprovalRequest(selectedTenantId, id, "current-admin");
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };
  const onReject = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await rejectApprovalRequest(selectedTenantId, id, "current-admin", "rejected");
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onToggle = async (action: string, enabled: boolean) => {
    if (!selectedTenantId) return;
    const next = { ...config, [action]: enabled };
    setConfig(next);
    try {
      await setApprovalConfig(selectedTenantId, next);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Admin approvals</h2>
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

      <h3>Gated actions</h3>
      <ul>
        {GATED_ACTIONS.map((a) => (
          <li key={a}>
            <label>
              <input
                type="checkbox"
                checked={Boolean(config[a])}
                onChange={(e) => onToggle(a, e.target.checked)}
              />{" "}
              {a}
            </label>
          </li>
        ))}
      </ul>

      <h3>Pending</h3>
      {pending.length === 0 ? <p>None.</p> : (
        <table>
          <thead>
            <tr><th>Action</th><th>Target</th><th>Requested by</th><th>Created</th><th /></tr>
          </thead>
          <tbody>
            {pending.map((r) => (
              <tr key={r.id}>
                <td>{r.action}</td>
                <td>{r.target_resource}</td>
                <td>{r.requester_id}</td>
                <td>{r.created_at}</td>
                <td>
                  <button type="button" onClick={() => onApprove(r.id)}>Approve</button>{" "}
                  <button type="button" onClick={() => onReject(r.id)}>Reject</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3>Recent history</h3>
      <table>
        <thead>
          <tr><th>Action</th><th>Target</th><th>Status</th><th>Resolved</th></tr>
        </thead>
        <tbody>
          {history.map((r) => (
            <tr key={r.id}>
              <td>{r.action}</td>
              <td>{r.target_resource}</td>
              <td>{r.status}</td>
              <td>{r.resolved_at ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
