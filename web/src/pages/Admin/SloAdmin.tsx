/**
 * SloAdmin renders the platform availability dashboard for the
 * Phase 4 99.9% target. It shows a platform-wide gauge, a per-
 * tenant availability table, P50/P95/P99 latency, and the SLO
 * breach history rolled up by `internal/monitoring/slo.go`.
 */

import { useEffect, useState } from "react";

import {
  getSloOverview,
  getTenantSlo,
  getSloBreaches,
  type SLOResponse,
  type SLOBreach,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function SloAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [overview, setOverview] = useState<SLOResponse | null>(null);
  const [tenantSlo, setTenantSlo] = useState<SLOResponse | null>(null);
  const [breaches, setBreaches] = useState<SLOBreach[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    getSloOverview()
      .then(setOverview)
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (!selectedTenantId) {
      setTenantSlo(null);
      setBreaches([]);
      return;
    }
    getTenantSlo(selectedTenantId)
      .then(setTenantSlo)
      .catch((e: unknown) => setError(String(e)));
    getSloBreaches(selectedTenantId)
      .then((b) => setBreaches(b ?? []))
      .catch((e: unknown) => setError(String(e)));
  }, [selectedTenantId]);

  return (
    <section className="kmail-admin-page kmail-slo-admin">
      <h2>Availability SLO</h2>
      <p>
        Target: <strong>99.9%</strong> over the trailing 24h. Source: KMail BFF
        request stream mirrored to Valkey by the metrics middleware.
      </p>

      <div className="kmail-slo-overview">
        <h3>Platform</h3>
        {overview ? <Card slo={overview} /> : <p>Loading…</p>}
      </div>

      <h3>Per-tenant</h3>
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
      {tenantSlo && <Card slo={tenantSlo} />}

      <h3>Recent breaches</h3>
      {breaches.length === 0 ? (
        <p>No breaches in the trailing 24h.</p>
      ) : (
        <table className="kmail-slo-breaches">
          <thead>
            <tr><th>Started</th><th>Ended</th><th>Availability</th></tr>
          </thead>
          <tbody>
            {breaches.map((b) => (
              <tr key={`${b.started_at}-${b.ended_at}`}>
                <td>{b.started_at}</td>
                <td>{b.ended_at}</td>
                <td>{(b.availability * 100).toFixed(2)}%</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {error && <p className="kmail-error">{error}</p>}
    </section>
  );
}

function Card({ slo }: { slo: SLOResponse }) {
  const a = slo.availability;
  const l = slo.latency;
  return (
    <div className="kmail-slo-card" style={{ display: "grid", gap: "0.5rem" }}>
      <p>
        Availability: <strong>{(a.availability * 100).toFixed(3)}%</strong> ({a.successes}/{a.total} successes, target {(a.target * 100).toFixed(1)}%)
      </p>
      <p>
        Latency p50/p95/p99: <strong>{l.p50_ms} / {l.p95_ms} / {l.p99_ms} ms</strong>
      </p>
    </div>
  );
}
