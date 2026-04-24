import { Fragment, useCallback, useEffect, useState } from "react";
import type { ChangeEvent } from "react";

import {
  AdminApiError,
  getDmarcSummary,
  listDmarcReports,
  uploadDmarcReport,
  type DMARCReport,
  type DMARCSummary,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * DmarcAdmin renders the DMARC aggregate report ingest results for
 * a tenant: per-domain pass/fail summary, recent report list with
 * drill-down into the raw record JSON, and a manual XML upload
 * form for offline ingestion.
 */
export default function DmarcAdmin() {
  const {
    tenants,
    selectedTenantId,
    selectTenant,
    isLoading: tenantsLoading,
    error: tenantsError,
  } = useTenantSelection();

  const [reports, setReports] = useState<DMARCReport[]>([]);
  const [summary, setSummary] = useState<DMARCSummary | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [uploading, setUploading] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);

  const load = useCallback((tenantId: string) => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    Promise.all([listDmarcReports(tenantId, { limit: 50 }), getDmarcSummary(tenantId)])
      .then(([rs, sum]) => {
        if (cancelled) return;
        setReports(rs);
        setSummary(sum);
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

  const onUpload = (e: ChangeEvent<HTMLInputElement>): void => {
    const file = e.target.files?.[0];
    if (!file || !selectedTenantId) return;
    setUploading(true);
    file
      .text()
      .then((xml) => uploadDmarcReport(selectedTenantId, xml))
      .then(() => load(selectedTenantId))
      .catch((err: unknown) => setError(errorMessage(err)))
      .finally(() => {
        setUploading(false);
        e.target.value = "";
      });
  };

  return (
    <section>
      <h2>DMARC reports</h2>

      <label>
        Tenant:&nbsp;
        <select
          value={selectedTenantId ?? ""}
          onChange={(e) => selectTenant(e.target.value)}
          disabled={tenantsLoading || !tenants?.length}
        >
          {tenants?.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name}
            </option>
          ))}
        </select>
      </label>

      {tenantsError && <p role="alert">Tenants: {tenantsError}</p>}
      {error && <p role="alert">{error}</p>}
      {loading && <p>Loading…</p>}

      {summary && (
        <div style={{ margin: "12px 0" }}>
          <h3>Last {summary.window_days} days</h3>
          <p>
            <strong>Pass rate:</strong> {(summary.pass_rate * 100).toFixed(1)}% (
            {summary.pass_count.toLocaleString()} pass /{" "}
            {summary.fail_count.toLocaleString()} fail over{" "}
            {summary.total.toLocaleString()} messages in {summary.report_count} reports)
          </p>
        </div>
      )}

      <h3>Upload aggregate XML report</h3>
      <input type="file" accept=".xml,application/xml,text/xml" onChange={onUpload} disabled={uploading} />
      {uploading && <span>&nbsp;Uploading…</span>}

      <h3>Recent reports</h3>
      <table>
        <thead>
          <tr>
            <th>Org</th>
            <th>Domain</th>
            <th>Window</th>
            <th>Pass</th>
            <th>Fail</th>
            <th>Policy</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {reports.map((r) => (
            <Fragment key={r.id}>
              <tr>
                <td>{r.org_name}</td>
                <td>{r.domain}</td>
                <td>
                  {new Date(r.date_range_begin).toLocaleDateString()}–
                  {new Date(r.date_range_end).toLocaleDateString()}
                </td>
                <td>{r.pass_count}</td>
                <td>{r.fail_count}</td>
                <td>{r.policy}</td>
                <td>
                  <button
                    type="button"
                    onClick={() => setExpanded((v) => (v === r.id ? null : r.id))}
                  >
                    {expanded === r.id ? "Hide" : "Details"}
                  </button>
                </td>
              </tr>
              {expanded === r.id && (
                <tr>
                  <td colSpan={7}>
                    <pre style={{ whiteSpace: "pre-wrap", fontSize: 12 }}>
                      {JSON.stringify(r.records, null, 2)}
                    </pre>
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
        </tbody>
      </table>
    </section>
  );
}

function errorMessage(e: unknown): string {
  if (e instanceof AdminApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}
