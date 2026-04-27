/**
 * SearchAdmin lets a tenant admin select between Meilisearch
 * (default) and OpenSearch as the search backend, and trigger a
 * reindex when switching.
 */
import { useCallback, useEffect, useState } from "react";

import {
  getSearchBackend,
  reindexSearch,
  setSearchBackend,
  type SearchBackendConfig,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function SearchAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [config, setConfig] = useState<SearchBackendConfig | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const reload = useCallback((tid: string) => {
    getSearchBackend(tid)
      .then(setConfig)
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onSelect = async (backend: SearchBackendConfig["backend"]) => {
    if (!selectedTenantId) return;
    setPending(true);
    try {
      const updated = await setSearchBackend(selectedTenantId, backend);
      setConfig(updated);
      // setSearchBackend only flips tenants.search_backend; it does
      // NOT enqueue a reindex. Make that explicit so operators don't
      // assume their data is being migrated and end up with empty
      // results on the new backend.
      setInfo(`Backend set to ${backend}. Click "Reindex now" to migrate existing data — the new backend will return empty results until a reindex completes.`);
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setPending(false);
    }
  };

  const onReindex = async () => {
    if (!selectedTenantId) return;
    setPending(true);
    try {
      await reindexSearch(selectedTenantId);
      setInfo("Reindex triggered.");
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="admin-page">
      <h2>Search backend</h2>
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
      </div>
      {error && <p className="error">{error}</p>}
      {info && <p className="info">{info}</p>}
      {config && (
        <div>
          <p>Current backend: <strong>{config.backend}</strong></p>
          <div className="actions">
            <button
              type="button"
              disabled={pending || config.backend === "meilisearch"}
              onClick={() => onSelect("meilisearch")}
            >
              Use Meilisearch
            </button>
            <button
              type="button"
              disabled={pending || config.backend === "opensearch"}
              onClick={() => onSelect("opensearch")}
            >
              Use OpenSearch
            </button>
            <button type="button" disabled={pending} onClick={onReindex}>
              Reindex now
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
