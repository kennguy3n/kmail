import { useCallback, useEffect, useMemo, useState } from "react";

import {
  AdminApiError,
  exportAuditLog,
  getAuditLog,
  verifyAuditChain,
  type AuditLogEntry,
  type AuditLogQuery,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * AuditAdmin renders the tamper-evident audit log backed by
 * `internal/audit/handlers.go`. Supports filtering, JSON/CSV
 * export, and hash-chain verification.
 */
export default function AuditAdmin() {
  const {
    tenants,
    selectedTenantId,
    selectTenant,
    isLoading: tenantsLoading,
    error: tenantsError,
  } = useTenantSelection();

  const [filters, setFilters] = useState<AuditLogQuery>({ limit: 100, offset: 0 });
  const [entries, setEntries] = useState<AuditLogEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [verifyStatus, setVerifyStatus] = useState<string | null>(null);

  const load = useCallback(
    (tenantId: string, f: AuditLogQuery) => {
      let cancelled = false;
      setLoading(true);
      setError(null);
      getAuditLog(tenantId, f)
        .then((list) => {
          if (cancelled) return;
          setEntries(list);
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
    },
    [],
  );

  useEffect(() => {
    if (!selectedTenantId) return;
    return load(selectedTenantId, filters);
  }, [selectedTenantId, filters, load]);

  const doExport = (format: "json" | "csv"): void => {
    if (!selectedTenantId) return;
    exportAuditLog(selectedTenantId, format, {
      since: filters.since,
      until: filters.until,
    })
      .then((text) => {
        const blob = new Blob([text], {
          type: format === "json" ? "application/json" : "text/csv",
        });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `audit-${selectedTenantId}.${format}`;
        a.click();
        URL.revokeObjectURL(url);
      })
      .catch((e: unknown) => setError(errorMessage(e)));
  };

  const doVerify = (): void => {
    if (!selectedTenantId) return;
    setVerifyStatus("Verifying…");
    verifyAuditChain(selectedTenantId)
      .then((r) => {
        setVerifyStatus(r.ok ? "Chain verified ✓" : `Chain broken: ${r.error}`);
      })
      .catch((e: unknown) => setVerifyStatus(errorMessage(e)));
  };

  const updateFilter = (k: keyof AuditLogQuery, v: string): void => {
    setFilters((f) => ({ ...f, [k]: v === "" ? undefined : v, offset: 0 }));
  };

  const rowSummary = useMemo(
    () => entries.map((e) => ({ ...e, when: new Date(e.created_at).toLocaleString() })),
    [entries],
  );

  return (
    <section>
      <h2>Audit log</h2>

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

      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (selectedTenantId) load(selectedTenantId, filters);
        }}
        style={{ display: "flex", gap: 8, flexWrap: "wrap", margin: "8px 0" }}
      >
        <input
          placeholder="Action"
          value={filters.action ?? ""}
          onChange={(e) => updateFilter("action", e.target.value)}
        />
        <input
          placeholder="Actor"
          value={filters.actor ?? ""}
          onChange={(e) => updateFilter("actor", e.target.value)}
        />
        <input
          placeholder="Resource type"
          value={filters.resource_type ?? ""}
          onChange={(e) => updateFilter("resource_type", e.target.value)}
        />
        <input
          type="datetime-local"
          value={filters.since ?? ""}
          onChange={(e) => updateFilter("since", e.target.value)}
        />
        <input
          type="datetime-local"
          value={filters.until ?? ""}
          onChange={(e) => updateFilter("until", e.target.value)}
        />
        <button type="submit">Apply</button>
        <button type="button" onClick={() => doExport("json")}>
          Export JSON
        </button>
        <button type="button" onClick={() => doExport("csv")}>
          Export CSV
        </button>
        <button type="button" onClick={doVerify}>
          Verify chain
        </button>
      </form>

      {verifyStatus && <p>{verifyStatus}</p>}
      {error && <p role="alert">{error}</p>}
      {loading && <p>Loading…</p>}

      <table>
        <thead>
          <tr>
            <th>When</th>
            <th>Action</th>
            <th>Actor</th>
            <th>Resource</th>
            <th>IP</th>
          </tr>
        </thead>
        <tbody>
          {rowSummary.map((e) => (
            <tr key={e.id}>
              <td>{e.when}</td>
              <td>{e.action}</td>
              <td>
                {e.actor_type}: {e.actor_id}
              </td>
              <td>
                {e.resource_type} {e.resource_id}
              </td>
              <td>{e.ip_address}</td>
            </tr>
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
