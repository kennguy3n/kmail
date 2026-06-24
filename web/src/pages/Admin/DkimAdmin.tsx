/**
 * DkimAdmin shows per-domain DKIM key history and lets an admin
 * manually rotate. The DNS wizard surfaces the new selector
 * record once a rotation is pending.
 */
import { useCallback, useEffect, useState } from "react";

import {
  listDkimKeys,
  listDomains,
  rotateDkimKey,
  type DkimKey,
  type TenantDomain,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function DkimAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [domains, setDomains] = useState<TenantDomain[]>([]);
  const [domainId, setDomainId] = useState<string>("");
  const [keys, setKeys] = useState<DkimKey[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  useEffect(() => {
    if (!selectedTenantId) return;
    listDomains(selectedTenantId)
      .then((ds) => {
        setDomains(ds);
        if (ds.length > 0) setDomainId(ds[0].id);
      })
      .catch((e: unknown) => setError(String(e)));
  }, [selectedTenantId]);

  const reload = useCallback(() => {
    if (!selectedTenantId || !domainId) return;
    listDkimKeys(selectedTenantId, domainId)
      .then((r) => setKeys(r.keys))
      .catch((e: unknown) => setError(String(e)));
  }, [selectedTenantId, domainId]);

  useEffect(() => {
    reload();
  }, [reload]);

  const onRotate = async () => {
    if (!selectedTenantId || !domainId) return;
    setPending(true);
    try {
      const k = await rotateDkimKey(selectedTenantId, domainId);
      setInfo(`Rotated; new selector ${k.selector}. Add the DNS record before traffic switches over.`);
      reload();
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="kmail-admin-page">
      <h2>DKIM keys</h2>
      <p className="kmail-admin-hint">View and rotate signing keys per domain. Add the new DNS record before traffic switches over.</p>
      <div className="kmail-admin-tenant-picker">
        <label>
          Tenant{" "}
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
        <label>
          Domain{" "}
          <select value={domainId} onChange={(e) => setDomainId(e.target.value)}>
            <option value="">— select —</option>
            {domains.map((d) => (
              <option key={d.id} value={d.id}>
                {d.domain}
              </option>
            ))}
          </select>
        </label>
        <button type="button" className="kmail-button" disabled={pending || !domainId} onClick={onRotate}>
          Rotate key
        </button>
      </div>
      {error && <p className="kmail-error">{error}</p>}
      {info && <p className="kmail-info">{info}</p>}
      <table className="kmail-admin-table">
        <thead>
          <tr>
            <th>Selector</th>
            <th>Status</th>
            <th>Created</th>
            <th>Activated</th>
            <th>Revoked</th>
          </tr>
        </thead>
        <tbody>
          {keys.map((k) => (
            <tr key={k.id}>
              <td className="font-mono text-xs">{k.selector}</td>
              <td>
                <span
                  className={
                    k.status === "active"
                      ? "kmail-flag-ok"
                      : k.status === "deprecated"
                        ? "kmail-flag-pending"
                        : "inline-flex rounded-pill bg-surface-muted px-2 py-0.5 text-xs font-semibold text-fg-muted"
                  }
                >
                  {k.status}
                </span>
              </td>
              <td>{k.created_at}</td>
              <td>{k.activated_at ?? "—"}</td>
              <td>{k.revoked_at ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
