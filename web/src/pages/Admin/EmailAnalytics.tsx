/**
 * EmailAnalytics is the Workstream 7 admin dashboard for per-user
 * email activity metrics. It surfaces:
 *
 *   • Totals: emails sent / received in the window
 *   • Daily volume chart (simple ASCII-bar table)
 *   • Top recipients and top senders
 *   • Busiest hours of the day
 *   • Average response time
 *
 * Data is fetched from `GET /api/v1/email-analytics?days=30` which
 * aggregates over the acting user's Sent + Inbox via JMAP (see
 * `internal/smartfeatures/analytics_service.go`). Tenant-wide
 * rollups across all accounts require audit / deliverability cross-
 * workstream integration and are documented as a follow-up.
 */

import { useCallback, useEffect, useState } from "react";

import {
  getEmailAnalytics,
  type EmailAnalytics as AnalyticsData,
} from "../../api/smart";
import { useTenantSelection } from "./useTenantSelection";

const PAGE_STYLE: React.CSSProperties = { padding: "1rem 2rem" };
const TABLE_STYLE: React.CSSProperties = {
  borderCollapse: "collapse",
  width: "100%",
  fontSize: "0.875rem",
  marginTop: "0.5rem",
};
const TH_STYLE: React.CSSProperties = {
  textAlign: "left",
  borderBottom: "2px solid #ccc",
  padding: "4px 8px",
};
const TD_STYLE: React.CSSProperties = {
  padding: "4px 8px",
  borderBottom: "1px solid #eee",
};
const CARD_STYLE: React.CSSProperties = {
  display: "inline-block",
  padding: "12px 20px",
  border: "1px solid #ddd",
  borderRadius: "8px",
  marginRight: "12px",
  marginBottom: "12px",
  textAlign: "center",
};
const NUM_STYLE: React.CSSProperties = { fontSize: "1.5rem", fontWeight: 700 };
const LABEL_STYLE: React.CSSProperties = {
  fontSize: "0.75rem",
  color: "#666",
  marginTop: 2,
};
const BAR_BASE: React.CSSProperties = {
  background: "#4c8bf5",
  height: "12px",
  borderRadius: "3px",
  transition: "width 0.3s ease",
};
const BAR_RECV: React.CSSProperties = {
  ...BAR_BASE,
  background: "#34a853",
};
const GRID_STYLE: React.CSSProperties = {
  display: "grid",
  gridTemplateColumns: "1fr 1fr",
  gap: "1rem",
  marginTop: "1rem",
};

export default function EmailAnalytics() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [data, setData] = useState<AnalyticsData | null>(null);
  const [days, setDays] = useState(30);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    setLoading(true);
    setError(null);
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    getEmailAnalytics({ days, tz, tenantId: selectedTenantId ?? undefined })
      .then(setData)
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, [days, selectedTenantId]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <section className="kmail-admin-page" style={PAGE_STYLE}>
      <h2>Email Analytics</h2>
      <p style={{ color: "#555", marginBottom: "0.5rem" }}>
        Activity dashboard for the authenticated user&rsquo;s mailbox.
        Tenant-wide rollups are a documented follow-up.
      </p>

      <div style={{ display: "flex", gap: "1rem", alignItems: "center", marginBottom: "1rem" }}>
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
          Window{" "}
          <select value={days} onChange={(e) => setDays(Number(e.target.value))}>
            <option value={7}>7 days</option>
            <option value={30}>30 days</option>
            <option value={90}>90 days</option>
            <option value={365}>365 days</option>
          </select>
        </label>

        <button type="button" onClick={load} disabled={loading}>
          {loading ? "Loading…" : "Refresh"}
        </button>
      </div>

      {error && <p style={{ color: "red" }}>{error}</p>}

      {data && (
        <>
          {/* ── KPI cards ──────────────────────────────────── */}
          <div style={{ marginBottom: "1rem" }}>
            <div style={CARD_STYLE}>
              <div style={NUM_STYLE}>{data.total_sent}</div>
              <div style={LABEL_STYLE}>Sent</div>
            </div>
            <div style={CARD_STYLE}>
              <div style={NUM_STYLE}>{data.total_received}</div>
              <div style={LABEL_STYLE}>Received</div>
            </div>
            <div style={CARD_STYLE}>
              <div style={NUM_STYLE}>
                {data.avg_response_seconds > 0
                  ? `${(data.avg_response_seconds / 3600).toFixed(1)}h`
                  : "—"}
              </div>
              <div style={LABEL_STYLE}>
                Avg response ({data.response_sample_size} threads)
              </div>
            </div>
            <div style={CARD_STYLE}>
              <div style={NUM_STYLE}>
                {data.range_start} — {data.range_end}
              </div>
              <div style={LABEL_STYLE}>Date range</div>
            </div>
          </div>

          {/* ── Daily chart ────────────────────────────────── */}
          <h3>Daily volume</h3>
          <table style={TABLE_STYLE}>
            <thead>
              <tr>
                <th style={TH_STYLE}>Date</th>
                <th style={TH_STYLE}>Sent</th>
                <th style={{ ...TH_STYLE, width: "40%" }}>Chart (blue=sent, green=recv)</th>
                <th style={TH_STYLE}>Received</th>
              </tr>
            </thead>
            <tbody>
              {data.daily.map((d) => {
                const maxDay = Math.max(
                  ...data.daily.map((dd) => dd.sent + dd.received),
                  1,
                );
                const sentPct = (d.sent / maxDay) * 100;
                const recvPct = (d.received / maxDay) * 100;
                return (
                  <tr key={d.date}>
                    <td style={TD_STYLE}>{d.date}</td>
                    <td style={TD_STYLE}>{d.sent}</td>
                    <td style={TD_STYLE}>
                      <div style={{ display: "flex", gap: 2 }}>
                        <div style={{ ...BAR_BASE, width: `${sentPct}%` }} />
                        <div style={{ ...BAR_RECV, width: `${recvPct}%` }} />
                      </div>
                    </td>
                    <td style={TD_STYLE}>{d.received}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>

          <div style={GRID_STYLE}>
            {/* ── Top recipients ──────────────────────────── */}
            <div>
              <h3>Top recipients</h3>
              <table style={TABLE_STYLE}>
                <thead>
                  <tr>
                    <th style={TH_STYLE}>#</th>
                    <th style={TH_STYLE}>Recipient</th>
                    <th style={TH_STYLE}>Emails</th>
                  </tr>
                </thead>
                <tbody>
                  {data.top_recipients.map((r, i) => (
                    <tr key={r.email}>
                      <td style={TD_STYLE}>{i + 1}</td>
                      <td style={TD_STYLE}>
                        {r.name ? `${r.name} <${r.email}>` : r.email}
                      </td>
                      <td style={TD_STYLE}>{r.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {/* ── Top senders ─────────────────────────────── */}
            <div>
              <h3>Top senders</h3>
              <table style={TABLE_STYLE}>
                <thead>
                  <tr>
                    <th style={TH_STYLE}>#</th>
                    <th style={TH_STYLE}>Sender</th>
                    <th style={TH_STYLE}>Emails</th>
                  </tr>
                </thead>
                <tbody>
                  {data.top_senders.map((s, i) => (
                    <tr key={s.email}>
                      <td style={TD_STYLE}>{i + 1}</td>
                      <td style={TD_STYLE}>
                        {s.name ? `${s.name} <${s.email}>` : s.email}
                      </td>
                      <td style={TD_STYLE}>{s.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>

          {/* ── Busiest hours ─────────────────────────────── */}
          <h3>Activity by hour (received messages)</h3>
          <div style={{ display: "flex", alignItems: "flex-end", gap: 2, height: 120, marginTop: "0.5rem" }}>
            {data.busiest_hours.map((h) => {
              const maxHour = Math.max(...data.busiest_hours.map((hh) => hh.count), 1);
              const pct = (h.count / maxHour) * 100;
              return (
                <div
                  key={h.hour}
                  title={`${h.hour}:00 — ${h.count} emails`}
                  style={{
                    flex: 1,
                    background: "#4c8bf5",
                    borderRadius: "3px 3px 0 0",
                    height: `${pct}%`,
                    minHeight: h.count > 0 ? 4 : 0,
                  }}
                />
              );
            })}
          </div>
          <div
            style={{
              display: "flex",
              gap: 2,
              fontSize: "0.65rem",
              color: "#888",
            }}
          >
            {data.busiest_hours.map((h) => (
              <div key={h.hour} style={{ flex: 1, textAlign: "center" }}>
                {h.hour}
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  );
}
