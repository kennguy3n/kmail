/**
 * ExportAdmin lets tenant admins create eDiscovery / data export
 * jobs and surfaces the per-job status + download URL once the
 * worker completes.
 */

import { useCallback, useEffect, useState } from "react";

import {
  createExport,
  listExports,
  type ExportJob,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function ExportAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [jobs, setJobs] = useState<ExportJob[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState<{ format: ExportJob["format"]; scope: ExportJob["scope"]; scope_ref: string }>({
    format: "mbox",
    scope: "all",
    scope_ref: "",
  });

  const reload = useCallback((tid: string) => {
    listExports(tid).then(setJobs).catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId) return;
    try {
      await createExport(selectedTenantId, "current-admin", draft.format, draft.scope, draft.scope_ref);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Tenant data exports</h2>

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

      {selectedTenantId && (
        <form onSubmit={onSubmit} style={{ display: "grid", gap: "0.5rem", maxWidth: 480 }}>
          <h3>New export</h3>
          <label>
            Format
            <select value={draft.format} onChange={(e) => setDraft({ ...draft, format: e.target.value as ExportJob["format"] })}>
              <option value="mbox">mbox (Phase 1)</option>
              <option value="eml">eml (per-message)</option>
              <option value="pst_stub">pst_stub (placeholder)</option>
            </select>
          </label>
          <label>
            Scope
            <select value={draft.scope} onChange={(e) => setDraft({ ...draft, scope: e.target.value as ExportJob["scope"] })}>
              <option value="all">All mail + calendar + audit</option>
              <option value="mailbox">Single mailbox</option>
              <option value="date_range">Date range</option>
            </select>
          </label>
          {(draft.scope === "mailbox" || draft.scope === "date_range") && (
            <label>
              Scope ref
              <input
                value={draft.scope_ref}
                onChange={(e) => setDraft({ ...draft, scope_ref: e.target.value })}
                placeholder={draft.scope === "mailbox" ? "mailbox_id" : "2025-01-01:2025-01-31"}
              />
            </label>
          )}
          <button type="submit">Queue export</button>
        </form>
      )}

      <h3 style={{ marginTop: "1.5rem" }}>Job history</h3>
      <table>
        <thead>
          <tr><th>Format</th><th>Scope</th><th>Status</th><th>Created</th><th>Download</th></tr>
        </thead>
        <tbody>
          {jobs.map((j) => (
            <tr key={j.id}>
              <td>{j.format}</td>
              <td>{j.scope}{j.scope_ref ? `:${j.scope_ref}` : ""}</td>
              <td>{j.status}</td>
              <td>{j.created_at}</td>
              <td>
                {j.download_url ? <a href={j.download_url}>Download</a> : "—"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
