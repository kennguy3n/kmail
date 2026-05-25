/**
 * SearchAdmin lets a tenant admin select which search backend
 * powers their mail search. Five values are supported (see
 * `SEARCH_BACKENDS`); the production default for newly-
 * provisioned tenants is `shared_meilisearch` (migration 050).
 *
 * The UI exposes each backend as a selectable card so the operator
 * can see ALL the alternatives at once — the old two-button
 * layout implicitly hid the shared / dedicated variants. After
 * flipping the backend column, the admin must explicitly trigger
 * `Reindex now` to migrate data; the BFF does not do that
 * automatically (the auto-cutover worker handles the
 * meili->opensearch and shared_meili->shared_opensearch paths in
 * the background, but a manual flip skips that worker).
 */
import { useCallback, useEffect, useState } from "react";

import {
  getSearchBackend,
  reindexSearch,
  SEARCH_BACKENDS,
  setSearchBackend,
  type SearchBackendConfig,
  type SearchBackendName,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

/**
 * Human-readable labels and one-line descriptions for each
 * backend. Centralised here so the strings stay in one file and
 * future backends only need an entry added.
 */
const BACKEND_DESCRIPTIONS: Record<SearchBackendName, { label: string; description: string }> = {
  meilisearch: {
    label: "Meilisearch (legacy per-tenant)",
    description: "Dedicated Meilisearch index for this tenant. Kept for backward compatibility — new tenants land on the shared variant by default.",
  },
  opensearch: {
    label: "OpenSearch (legacy per-tenant)",
    description: "Dedicated OpenSearch index for this tenant. Kept for backward compatibility.",
  },
  shared_meilisearch: {
    label: "Shared Meilisearch (default)",
    description: "Index keyed by Stalwart shard; tenants on the same shard share one Meilisearch index, isolated via a tenant_id filter at query time.",
  },
  shared_opensearch: {
    label: "Shared OpenSearch",
    description: "Auto-cutover target when a shared-Meilisearch tenant outgrows the single-node ceiling. Same shard-keyed shape, OpenSearch implementation.",
  },
  dedicated_opensearch: {
    label: "Dedicated OpenSearch (enterprise)",
    description: "Per-tenant OpenSearch index. Provisioned manually for enterprise plans that require physical index isolation.",
  },
};

export default function SearchAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [config, setConfig] = useState<SearchBackendConfig | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const reload = useCallback((tid: string) => {
    setError(null);
    getSearchBackend(tid)
      .then(setConfig)
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onSelect = async (backend: SearchBackendName) => {
    if (!selectedTenantId) return;
    setPending(true);
    setError(null);
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
    setError(null);
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
          <p>
            Current backend: <strong>{config.backend}</strong>{" "}
            <span className="muted">({BACKEND_DESCRIPTIONS[config.backend]?.label ?? "unknown"})</span>
          </p>
          <ul className="backend-options" role="radiogroup" aria-label="Search backend">
            {SEARCH_BACKENDS.map((backend) => {
              const desc = BACKEND_DESCRIPTIONS[backend];
              const isCurrent = config.backend === backend;
              return (
                <li key={backend} className={isCurrent ? "current" : ""}>
                  <button
                    type="button"
                    role="radio"
                    aria-checked={isCurrent}
                    disabled={pending || isCurrent}
                    onClick={() => onSelect(backend)}
                  >
                    <strong>{desc.label}</strong>
                    <span className="backend-name muted">{backend}</span>
                    <span className="backend-description">{desc.description}</span>
                  </button>
                </li>
              );
            })}
          </ul>
          <div className="actions">
            <button type="button" disabled={pending} onClick={onReindex}>
              Reindex now
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
