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
    <div className="admin-page">
      <h2>DKIM keys</h2>
      <div className="tenant-picker">
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
        <button type="button" disabled={pending || !domainId} onClick={onRotate}>
          Rotate key
        </button>
      </div>
      {error && <p className="error">{error}</p>}
      {info && <p className="info">{info}</p>}
      <table className="admin-table">
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
              <td>{k.selector}</td>
              <td>{k.status}</td>
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
