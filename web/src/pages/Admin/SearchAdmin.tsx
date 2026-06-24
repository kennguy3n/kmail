/**
 * SearchAdmin lets a tenant admin select which search backend
 * powers their mail search. Five values are supported (see
 * `SEARCH_BACKENDS`); the production default for newly-
 * provisioned tenants is `shared_meilisearch` (per the
 * `tenants.search_backend` default in `migrations/001_baseline.sql`).
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
  ADMIN_API_BASE,
  adminAuthHeaders,
  getSearchBackend,
  listAvailableSearchBackends,
  reindexSearch,
  requestJSON,
  SEARCH_BACKENDS,
  setSearchBackend,
  type SearchBackendConfig,
  type SearchBackendName,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

/**
 * One row of `search_cutover_jobs` as serialised by
 * `internal/search/cutover.go#CutoverJob`. A tenant can carry one
 * row per target backend, so the history is keyed by
 * (tenant, target_backend).
 */
export interface CutoverJob {
  tenant_id: string;
  target_backend: SearchBackendName;
  cutover_state: "pending" | "in_progress" | "completed" | "failed";
  mailbox_size: number;
  threshold: number;
  started_at?: string;
  completed_at?: string;
  failure_count: number;
  last_error?: string;
  created_at: string;
  updated_at: string;
}

/**
 * listCutoverJobs returns the tenant's cutover history across all
 * targets. The endpoint wraps the array in `{ jobs }`; we unwrap to
 * the bare slice (and tolerate a missing/`null` field) so callers
 * always get an array.
 */
async function listCutoverJobs(tenantId: string): Promise<CutoverJob[]> {
  const res = await requestJSON<{ jobs: CutoverJob[] | null }>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/search/cutover`,
    { headers: adminAuthHeaders(tenantId) },
  );
  return res.jobs ?? [];
}

/**
 * initiateCutover records operator intent and synchronously runs
 * the migration, returning the terminal job row. A reindex/
 * validation failure surfaces as a thrown AdminApiError and leaves
 * the tenant on its source backend.
 */
async function initiateCutover(
  tenantId: string,
  targetBackend: SearchBackendName,
): Promise<CutoverJob> {
  return requestJSON<CutoverJob>(
    `${ADMIN_API_BASE}/tenants/${encodeURIComponent(tenantId)}/search/cutover`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, { "Content-Type": "application/json" }),
      body: JSON.stringify({ target_backend: targetBackend }),
    },
  );
}

/** Human-readable label for a cutover state. */
const CUTOVER_STATE_LABEL: Record<CutoverJob["cutover_state"], string> = {
  pending: "Pending",
  in_progress: "In progress",
  completed: "Completed",
  failed: "Failed",
};

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
  // `available` is the set of backend names this BFF actually has
  // a Go implementation for. The CHECK constraint admits values
  // like `dedicated_opensearch` that not every deployment ships
  // — so we disable those cards rather than letting the operator
  // flip a tenant onto a backend that would 404 on every search.
  // `null` until the fetch completes; we disable ALL cards in
  // that initial state so a fast-clicker can't beat the gate.
  const [available, setAvailable] = useState<Set<SearchBackendName> | null>(null);
  // Cutover history + manual-trigger state. `null` until the first
  // fetch completes so the table can show a loading affordance.
  const [cutoverJobs, setCutoverJobs] = useState<CutoverJob[] | null>(null);
  const [cutoverTarget, setCutoverTarget] = useState<SearchBackendName | "">("");
  const [cutoverPending, setCutoverPending] = useState(false);

  const reloadCutover = useCallback((tid: string) => {
    listCutoverJobs(tid)
      .then(setCutoverJobs)
      .catch((e: unknown) => setError(String(e)));
  }, []);

  const reload = useCallback(
    (tid: string) => {
      setError(null);
      // Reset the manual-cutover target on every (re)load. Without
      // this the picker keeps the value chosen for the PREVIOUS
      // tenant across a tenant switch: the stale option is filtered
      // out of the <select> but the controlled value survives, so
      // the "Start cutover" button stays enabled and would submit a
      // target meant for a different tenant. (Tenant-switch-only
      // state — `info` and `cutoverJobs` — is cleared in the
      // selectedTenantId effect, not here, so a same-tenant refresh
      // after a cutover keeps its success line.)
      setCutoverTarget("");
      getSearchBackend(tid)
        .then(setConfig)
        .catch((e: unknown) => setError(String(e)));
      reloadCutover(tid);
    },
    [reloadCutover],
  );

  useEffect(() => {
    if (!selectedTenantId) return;
    // A new tenant was selected: drop the previous tenant's transient
    // view state before the refetch lands so nothing bleeds across the
    // switch. `cutoverJobs -> null` falls the history table back to its
    // "Loading…" affordance instead of briefly showing tenant A's rows
    // under tenant B; `info` clears a "Cutover to X completed." line so
    // it can't linger under a different tenant. (Same-tenant refreshes
    // call `reload` directly and intentionally preserve these.)
    setInfo(null);
    setCutoverJobs(null);
    reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  // Fetch the wired-backend list once per mount. The endpoint is
  // not tenant-scoped (deployment-wide config), so we do not
  // re-fetch when `selectedTenantId` changes.
  useEffect(() => {
    listAvailableSearchBackends()
      .then((names) => setAvailable(new Set(names)))
      .catch((e: unknown) => setError(String(e)));
  }, []);

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

  // onCutover synchronously migrates the tenant to the chosen
  // target backend (export → reindex → validate → flip). The button
  // blocks with a progress indicator until the BFF returns the
  // terminal job; a validation failure throws and leaves the tenant
  // safely on its source backend, which we surface as an error.
  const onCutover = async () => {
    if (!selectedTenantId || !cutoverTarget) return;
    setCutoverPending(true);
    setError(null);
    setInfo(null);
    try {
      const job = await initiateCutover(selectedTenantId, cutoverTarget);
      setInfo(`Cutover to ${job.target_backend} ${CUTOVER_STATE_LABEL[job.cutover_state].toLowerCase()}.`);
      setCutoverTarget("");
      // The backend column moved; refresh both the current-backend
      // card and the history table.
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
      // Even on failure the history table now carries a `failed`
      // row — refresh so the operator sees it.
      reloadCutover(selectedTenantId);
    } finally {
      setCutoverPending(false);
    }
  };

  return (
    <div className="kmail-admin-page">
      <h2>Search backend</h2>
      <p className="kmail-admin-hint">Choose the search index for this tenant and run cutovers or reindexes.</p>
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
      </div>
      {error && <p className="kmail-error">{error}</p>}
      {info && <p className="kmail-info">{info}</p>}
      {config && (
        <div>
          <p className="mb-3 text-sm">
            Current backend: <strong className="text-fg">{config.backend}</strong>{" "}
            <span className="muted">({BACKEND_DESCRIPTIONS[config.backend]?.label ?? "unknown"})</span>
          </p>
          <ul className="backend-options grid gap-3" role="radiogroup" aria-label="Search backend">
            {SEARCH_BACKENDS.map((backend) => {
              const desc = BACKEND_DESCRIPTIONS[backend];
              const isCurrent = config.backend === backend;
              // Disable until the availability lookup returns so
              // an operator clicking faster than the network can
              // settle can't slip past the gate.
              const isAvailable = available !== null && available.has(backend);
              const isDisabled = pending || isCurrent || !isAvailable;
              return (
                <li key={backend} className={isCurrent ? "current" : ""}>
                  <button
                    type="button"
                    role="radio"
                    aria-checked={isCurrent}
                    aria-disabled={isDisabled}
                    disabled={isDisabled}
                    onClick={() => onSelect(backend)}
                    title={!isAvailable && available !== null ? "Not available in this deployment — no implementation wired into this BFF." : undefined}
                  >
                    <strong>{desc.label}</strong>
                    <span className="backend-name muted">{backend}</span>
                    <span className="backend-description">{desc.description}</span>
                    {!isAvailable && available !== null && (
                      <span className="backend-unavailable muted">Not available in this deployment</span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
          <div className="kmail-actions">
            <button type="button" disabled={pending} onClick={onReindex}>
              Reindex now
            </button>
          </div>

          <section className="cutover-section" aria-labelledby="cutover-heading">
            <h3 id="cutover-heading">Cutover</h3>
            <p className="muted">
              Migrate this tenant&apos;s search index to another backend (export → reindex → validate → flip). A
              failed validation leaves the tenant on its current backend. The auto-cutover worker performs the same
              migration in the background once a mailbox outgrows the threshold.
            </p>
            <div className="actions cutover-trigger">
              <label>
                Target backend{" "}
                <select
                  value={cutoverTarget}
                  disabled={cutoverPending}
                  onChange={(e) => setCutoverTarget(e.target.value as SearchBackendName | "")}
                >
                  <option value="">— select —</option>
                  {SEARCH_BACKENDS.filter(
                    (b) => b !== config.backend && available !== null && available.has(b),
                  ).map((b) => (
                    <option key={b} value={b}>
                      {BACKEND_DESCRIPTIONS[b]?.label ?? b}
                    </option>
                  ))}
                </select>
              </label>
              <button
                type="button"
                disabled={cutoverPending || cutoverTarget === ""}
                onClick={onCutover}
              >
                Start cutover
              </button>
              {cutoverPending && (
                <span className="cutover-progress" role="status">
                  Cutover in progress…
                </span>
              )}
            </div>

            <h4>History</h4>
            {cutoverJobs === null ? (
              <p className="muted">Loading…</p>
            ) : cutoverJobs.length === 0 ? (
              <p className="muted">No cutovers have run for this tenant.</p>
            ) : (
              <table className="cutover-history">
                <thead>
                  <tr>
                    <th>Target backend</th>
                    <th>State</th>
                    <th>Started</th>
                    <th>Completed</th>
                    <th>Failures</th>
                    <th>Last error</th>
                  </tr>
                </thead>
                <tbody>
                  {cutoverJobs.map((job) => (
                    <tr key={job.target_backend} className={`cutover-${job.cutover_state}`}>
                      <td>{job.target_backend}</td>
                      <td>{CUTOVER_STATE_LABEL[job.cutover_state]}</td>
                      <td>{job.started_at ? new Date(job.started_at).toLocaleString() : "—"}</td>
                      <td>{job.completed_at ? new Date(job.completed_at).toLocaleString() : "—"}</td>
                      <td>{job.failure_count}</td>
                      <td className="cutover-error">{job.last_error || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </section>
        </div>
      )}
    </div>
  );
}
