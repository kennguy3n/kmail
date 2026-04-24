import { Fragment, useCallback, useEffect, useState } from "react";

import {
  AdminApiError,
  getDomainRecords,
  listDomains,
  verifyDomain,
  type DomainRecords,
  type DomainVerificationResult,
  type TenantDomain,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * DomainAdmin is the domain management console.
 *
 * Drives the DNS Onboarding wizard for the currently-selected
 * tenant: lists domains, shows the four per-check verification
 * flags (MX / SPF / DKIM / DMARC) off the `domains` table, and
 * exposes a per-row **Verify** action that re-runs the live DNS
 * checks through `POST /api/v1/tenants/:id/domains/:domainId/verify`.
 * The expected DNS records (MX / SPF / DKIM / DMARC / MTA-STS /
 * TLS-RPT / autoconfig) come from
 * `GET /api/v1/tenants/:id/domains/:domainId/dns-records` and
 * render in an expandable panel per domain so the tenant can copy
 * them into their authoritative DNS before re-running the verify
 * step. See docs/PROPOSAL.md §9.3 and docs/SCHEMA.md §5.3.
 */
export default function DomainAdmin() {
  const {
    tenants,
    selectedTenantId,
    selectedTenant,
    selectTenant,
    isLoading: tenantsLoading,
    error: tenantsError,
  } = useTenantSelection();

  const [domains, setDomains] = useState<TenantDomain[] | null>(null);
  const [domainsLoading, setDomainsLoading] = useState(false);
  const [domainsError, setDomainsError] = useState<string | null>(null);

  // Per-domain verify status: "idle" | "running" | "ok" | "failed".
  // `running` disables the button and shows a running indicator;
  // `ok` / `failed` are transient and clear on the next reload so a
  // stale result does not hide the persisted per-check flags.
  const [verifyState, setVerifyState] = useState<
    Record<string, { status: "idle" | "running" | "ok" | "failed"; message?: string }>
  >({});

  // Per-domain expanded DNS-records panel. `records` is cached so
  // collapsing and re-expanding does not re-fetch; `loading` and
  // `error` mirror the fetch status.
  const [records, setRecords] = useState<Record<string, DomainRecords>>({});
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [recordsLoading, setRecordsLoading] = useState<Record<string, boolean>>(
    {},
  );
  const [recordsError, setRecordsError] = useState<Record<string, string>>({});

  const loadDomains = useCallback(
    (tenantId: string) => {
      let cancelled = false;
      setDomainsLoading(true);
      setDomainsError(null);
      listDomains(tenantId)
        .then((list) => {
          if (cancelled) return;
          setDomains(list);
        })
        .catch((e: unknown) => {
          if (cancelled) return;
          setDomainsError(errorMessage(e));
        })
        .finally(() => {
          if (cancelled) return;
          setDomainsLoading(false);
        });
      return () => {
        cancelled = true;
      };
    },
    [],
  );

  useEffect(() => {
    if (!selectedTenantId) return;
    return loadDomains(selectedTenantId);
  }, [selectedTenantId, loadDomains]);

  const onVerify = (domain: TenantDomain): void => {
    if (!selectedTenantId) return;
    setVerifyState((prev) => ({
      ...prev,
      [domain.id]: { status: "running" },
    }));
    verifyDomain(selectedTenantId, domain.id)
      .then((result: DomainVerificationResult) => {
        setVerifyState((prev) => ({
          ...prev,
          [domain.id]: {
            status: result.verified ? "ok" : "failed",
            message: summarizeResult(result),
          },
        }));
        // Refresh the list so the persisted per-check flags catch
        // up with what `verifyDomain` just wrote.
        loadDomains(selectedTenantId);
      })
      .catch((e: unknown) => {
        setVerifyState((prev) => ({
          ...prev,
          [domain.id]: { status: "failed", message: errorMessage(e) },
        }));
      });
  };

  const onToggleRecords = (domain: TenantDomain): void => {
    if (!selectedTenantId) return;
    setExpanded((prev) => ({ ...prev, [domain.id]: !prev[domain.id] }));
    if (records[domain.id]) return;
    setRecordsLoading((prev) => ({ ...prev, [domain.id]: true }));
    setRecordsError((prev) => {
      const { [domain.id]: _dropped, ...rest } = prev;
      return rest;
    });
    getDomainRecords(selectedTenantId, domain.id)
      .then((r) => {
        setRecords((prev) => ({ ...prev, [domain.id]: r }));
      })
      .catch((e: unknown) => {
        setRecordsError((prev) => ({
          ...prev,
          [domain.id]: errorMessage(e),
        }));
      })
      .finally(() => {
        setRecordsLoading((prev) => ({ ...prev, [domain.id]: false }));
      });
  };

  return (
    <section className="kmail-admin">
      <h2>Domain admin</h2>

      <TenantPicker
        tenants={tenants}
        selectedTenantId={selectedTenantId}
        onSelect={selectTenant}
        isLoading={tenantsLoading}
        error={tenantsError}
      />

      {!selectedTenant ? (
        <p className="kmail-admin-hint">
          {tenantsLoading
            ? "Loading tenants…"
            : "Select a tenant to manage its domains."}
        </p>
      ) : (
        <>
          <p className="kmail-admin-hint">
            Domains for <strong>{selectedTenant.name}</strong> ({selectedTenant.slug}).
          </p>

          {domainsLoading && <p>Loading domains…</p>}
          {domainsError && (
            <p className="kmail-admin-error">Failed to load domains: {domainsError}</p>
          )}

          {domains && domains.length === 0 && !domainsLoading && (
            <p>No domains configured for this tenant.</p>
          )}

          {domains && domains.length > 0 && (
            <table className="kmail-admin-table">
              <thead>
                <tr>
                  <th>Domain</th>
                  <th>MX</th>
                  <th>SPF</th>
                  <th>DKIM</th>
                  <th>DMARC</th>
                  <th>Overall</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {domains.map((d) => {
                  const v = verifyState[d.id];
                  const isExpanded = !!expanded[d.id];
                  return (
                    <Fragment key={d.id}>
                      <tr>
                        <td>{d.domain}</td>
                        <td><Flag ok={d.mx_verified} /></td>
                        <td><Flag ok={d.spf_verified} /></td>
                        <td><Flag ok={d.dkim_verified} /></td>
                        <td><Flag ok={d.dmarc_verified} /></td>
                        <td><Flag ok={d.verified} label={d.verified ? "verified" : "pending"} /></td>
                        <td>
                          <button
                            type="button"
                            onClick={() => onVerify(d)}
                            disabled={v?.status === "running"}
                          >
                            {v?.status === "running" ? "Verifying…" : "Verify"}
                          </button>{" "}
                          <button
                            type="button"
                            onClick={() => onToggleRecords(d)}
                            aria-expanded={isExpanded}
                          >
                            {isExpanded ? "Hide DNS records" : "Show DNS records"}
                          </button>
                          {v?.message && (
                            <div
                              className={
                                v.status === "ok"
                                  ? "kmail-admin-note"
                                  : "kmail-admin-error"
                              }
                            >
                              {v.message}
                            </div>
                          )}
                        </td>
                      </tr>
                      {isExpanded && (
                        <tr>
                          <td colSpan={7}>
                            <DomainRecordsPanel
                              loading={!!recordsLoading[d.id]}
                              error={recordsError[d.id]}
                              records={records[d.id]}
                            />
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          )}
        </>
      )}
    </section>
  );
}

function Flag({ ok, label }: { ok: boolean; label?: string }): JSX.Element {
  const text = label ?? (ok ? "ok" : "pending");
  return (
    <span className={ok ? "kmail-flag-ok" : "kmail-flag-pending"}>{text}</span>
  );
}

function summarizeResult(r: DomainVerificationResult): string {
  if (r.verified) return "All checks passed.";
  const failing = [
    !r.mx_verified && "MX",
    !r.spf_verified && "SPF",
    !r.dkim_verified && "DKIM",
    !r.dmarc_verified && "DMARC",
  ].filter(Boolean) as string[];
  return `Still pending: ${failing.join(", ")}.`;
}

function errorMessage(e: unknown): string {
  if (e instanceof AdminApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}

function DomainRecordsPanel({
  loading,
  error,
  records,
}: {
  loading: boolean;
  error?: string;
  records?: DomainRecords;
}): JSX.Element {
  if (loading) return <p>Loading DNS records…</p>;
  if (error) return <p className="kmail-admin-error">{error}</p>;
  if (!records) return <p>No records available.</p>;
  return (
    <div>
      <p className="kmail-admin-hint">
        Publish the following records on the authoritative DNS for{" "}
        <code>{records.domain}</code>, then hit <strong>Verify</strong>.
      </p>
      <table className="kmail-admin-table kmail-admin-subtable">
        <thead>
          <tr>
            <th>Type</th>
            <th>Name</th>
            <th>Value</th>
            <th>TTL</th>
            <th>Priority</th>
            <th>Notes</th>
          </tr>
        </thead>
        <tbody>
          {records.records.map((r, i) => (
            <tr key={`${r.type}-${r.name}-${i}`}>
              <td>{r.type}</td>
              <td><code>{r.name}</code></td>
              <td><code>{r.value}</code></td>
              <td>{r.ttl ?? ""}</td>
              <td>{r.priority ?? ""}</td>
              <td>{r.notes ?? ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function TenantPicker({
  tenants,
  selectedTenantId,
  onSelect,
  isLoading,
  error,
}: {
  tenants: { id: string; name: string; slug: string }[] | null;
  selectedTenantId: string | null;
  onSelect: (id: string) => void;
  isLoading: boolean;
  error: string | null;
}): JSX.Element {
  return (
    <div className="kmail-admin-tenant-picker">
      <label htmlFor="kmail-admin-tenant">Tenant:</label>{" "}
      <select
        id="kmail-admin-tenant"
        disabled={isLoading || !tenants || tenants.length === 0}
        value={selectedTenantId ?? ""}
        onChange={(e) => onSelect(e.target.value)}
      >
        {isLoading && <option value="">Loading…</option>}
        {!isLoading && tenants && tenants.length === 0 && (
          <option value="">No tenants</option>
        )}
        {tenants &&
          tenants.map((t) => (
            <option key={t.id} value={t.id}>
              {t.name} ({t.slug})
            </option>
          ))}
      </select>
      {error && (
        <span className="kmail-admin-error"> Failed to load tenants: {error}</span>
      )}
    </div>
  );
}
