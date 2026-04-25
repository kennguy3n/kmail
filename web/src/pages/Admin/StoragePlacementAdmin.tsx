/**
 * StoragePlacementAdmin lets tenant admins configure where data
 * lives via the zk-object-fabric placement policy. Policy changes
 * write through to the fabric console API; existing data is NOT
 * migrated automatically (the warning banner spells this out).
 */

import { useCallback, useEffect, useState } from "react";

import {
  getPlacementPolicy,
  listRegions,
  updatePlacementPolicy,
  type PlacementPolicy,
  type AvailableRegion,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function StoragePlacementAdmin() {
  const { tenants, selectedTenantId, selectedTenant, selectTenant } = useTenantSelection();
  const [policy, setPolicy] = useState<PlacementPolicy | null>(null);
  const [regions, setRegions] = useState<AvailableRegion[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    getPlacementPolicy(tid).then(setPolicy).catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    listRegions().then(setRegions).catch(() => undefined);
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onSave = async () => {
    if (!selectedTenantId || !policy) return;
    setError(null);
    setInfo(null);
    try {
      const next = await updatePlacementPolicy(selectedTenantId, policy);
      setPolicy(next);
      setInfo("Placement policy updated. Existing data is not automatically migrated.");
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const planAllowsStrictZK = selectedTenant?.plan === "privacy";

  return (
    <section className="kmail-admin-page">
      <h2>Storage placement</h2>
      <p>
        Configure where this tenant&rsquo;s mail and attachments live in
        zk-object-fabric. Changes apply to <em>new</em> data only — existing
        objects stay where they are until a migration job runs.
      </p>

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
      {info && <p className="kmail-info">{info}</p>}
      {policy && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void onSave();
          }}
          style={{ display: "grid", gap: "0.5rem", maxWidth: 480 }}
        >
          <fieldset>
            <legend>Allowed regions</legend>
            {regions.map((r) => (
              <label key={r.code} style={{ display: "block" }}>
                <input
                  type="checkbox"
                  checked={policy.countries.includes(r.code)}
                  onChange={(e) => {
                    const set = new Set(policy.countries);
                    if (e.target.checked) set.add(r.code);
                    else set.delete(r.code);
                    setPolicy({ ...policy, countries: Array.from(set) });
                  }}
                />
                {r.name} ({r.code})
              </label>
            ))}
          </fieldset>

          <label>
            Preferred provider
            <input
              value={policy.preferred_provider ?? ""}
              onChange={(e) => setPolicy({ ...policy, preferred_provider: e.target.value })}
            />
          </label>

          <label>
            Encryption mode
            <select
              value={policy.encryption_mode ?? "managed"}
              onChange={(e) => setPolicy({ ...policy, encryption_mode: e.target.value })}
            >
              <option value="managed">Managed (ManagedEncrypted)</option>
              <option value="client_side" disabled={!planAllowsStrictZK}>
                Client-side (StrictZK) {planAllowsStrictZK ? "" : "— privacy plan only"}
              </option>
            </select>
          </label>

          <label>
            Erasure profile
            <input
              value={policy.erasure_profile ?? ""}
              onChange={(e) => setPolicy({ ...policy, erasure_profile: e.target.value })}
            />
          </label>

          <button type="submit">Save placement policy</button>
        </form>
      )}
    </section>
  );
}
