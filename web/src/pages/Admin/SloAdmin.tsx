/**
 * SloAdmin renders the platform availability dashboard. The
 * Phase 5 target is 99.95% across all regions; the gauge shows
 * both the legacy 99.9% line (dashed) and the new 99.95% target
 * line (solid). Per-region rollups come from
 * `/api/v1/admin/slo/regions` (multi-region aggregator). Source:
 * KMail BFF request stream mirrored to Valkey by the metrics
 * middleware.
 */

import { useEffect, useMemo, useState } from "react";

import {
  getSloOverview,
  getTenantSlo,
  getSloBreaches,
  getSloRegions,
  type SLOResponse,
  type SLOBreach,
  type MultiRegionResult,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

/** Phase 5 target. Mirrors `monitoring.HighAvailabilityTarget`. */
const HA_TARGET = 0.9995;
/** Phase 4 baseline. Mirrors `monitoring.LegacyTarget`. */
const LEGACY_TARGET = 0.999;

export default function SloAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [overview, setOverview] = useState<SLOResponse | null>(null);
  const [tenantSlo, setTenantSlo] = useState<SLOResponse | null>(null);
  const [breaches, setBreaches] = useState<SLOBreach[]>([]);
  const [regions, setRegions] = useState<MultiRegionResult | null>(null);
  const [selectedRegion, setSelectedRegion] = useState<string>("");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    getSloOverview()
      .then(setOverview)
      .catch((e: unknown) => setError(String(e)));
    getSloRegions()
      .then(setRegions)
      .catch((e: unknown) => setError(String(e)));
  }, []);

  const filteredRegions = useMemo(() => {
    if (!regions) return [] as MultiRegionResult["regions"];
    if (!selectedRegion) return regions.regions;
    return regions.regions.filter((r) => r.region === selectedRegion);
  }, [regions, selectedRegion]);

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
        Phase 5 target: <strong>{(HA_TARGET * 100).toFixed(2)}%</strong> over
        the trailing 24h. Legacy Phase 4 baseline:{" "}
        <strong>{(LEGACY_TARGET * 100).toFixed(1)}%</strong>. Source: KMail BFF
        request stream mirrored to Valkey by the metrics middleware.
      </p>

      <div className="kmail-slo-overview">
        <h3>Platform</h3>
        {overview ? <Card slo={overview} /> : <p>Loading…</p>}
      </div>

      <h3>Regions</h3>
      <div className="kmail-admin-controls">
        <label>
          Region
          <select
            value={selectedRegion}
            onChange={(e) => setSelectedRegion(e.target.value)}
          >
            <option value="">All regions</option>
            {(regions?.regions ?? []).map((r) => (
              <option key={r.region} value={r.region}>{r.region}</option>
            ))}
          </select>
        </label>
      </div>
      {regions && regions.regions.length > 0 ? (
        <table className="kmail-slo-regions">
          <thead>
            <tr>
              <th>Region</th>
              <th>Total</th>
              <th>Successes</th>
              <th>Failures</th>
              <th>Availability</th>
              <th>Target</th>
            </tr>
          </thead>
          <tbody>
            {filteredRegions.map((r) => (
              <tr key={r.region}>
                <td>{r.region}</td>
                <td>{r.total}</td>
                <td>{r.successes}</td>
                <td>{r.failures}</td>
                <td className={r.availability < r.target ? "kmail-slo-breach" : ""}>
                  {(r.availability * 100).toFixed(3)}%
                </td>
                <td>{(r.target * 100).toFixed(2)}%</td>
              </tr>
            ))}
          </tbody>
          <tfoot>
            <tr>
              <td><strong>Global</strong></td>
              <td>{regions.global_total}</td>
              <td>{regions.global_success}</td>
              <td>{regions.global_failures}</td>
              <td>{(regions.global_availability * 100).toFixed(3)}%</td>
              <td>{(regions.target * 100).toFixed(2)}%</td>
            </tr>
          </tfoot>
        </table>
      ) : (
        <p>No region data available.</p>
      )}

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
  const meetsHA = a.availability >= HA_TARGET;
  const meetsLegacy = a.availability >= LEGACY_TARGET;
  return (
    <div className="kmail-slo-card" style={{ display: "grid", gap: "0.5rem" }}>
      <p>
        Availability: <strong>{(a.availability * 100).toFixed(3)}%</strong> (
        {a.successes}/{a.total} successes)
      </p>
      <p>
        Phase 5 target ({(HA_TARGET * 100).toFixed(2)}%):{" "}
        <strong className={meetsHA ? "kmail-slo-ok" : "kmail-slo-breach"}>
          {meetsHA ? "meeting" : "below"}
        </strong>
        {" · "}
        Phase 4 target ({(LEGACY_TARGET * 100).toFixed(1)}%):{" "}
        <strong className={meetsLegacy ? "kmail-slo-ok" : "kmail-slo-breach"}>
          {meetsLegacy ? "meeting" : "below"}
        </strong>
      </p>
      <p>
        Latency p50/p95/p99: <strong>{l.p50_ms} / {l.p95_ms} / {l.p99_ms} ms</strong>
      </p>
    </div>
  );
}
