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

import { cn } from "../../lib/cn";

import {
  getEmailAnalytics,
  type EmailAnalytics as AnalyticsData,
} from "../../api/smart";
import { useTenantSelection } from "./useTenantSelection";

const PAGE_STYLE = "px-8 py-4";
const TABLE_STYLE = "mt-2 w-full border-collapse text-sm";
const TH_STYLE = "border-b-2 border-border px-2 py-1 text-left";
const TD_STYLE = "border-b border-border px-2 py-1";
const CARD_STYLE =
  "mb-3 mr-3 inline-block rounded-lg border border-border bg-surface px-5 py-3 text-center";
const NUM_STYLE = "text-2xl font-bold";
const LABEL_STYLE = "mt-0.5 text-xs text-fg-muted";
const BAR_BASE = "h-3 rounded-[3px] bg-info transition-[width] duration-300";
const BAR_RECV = "h-3 rounded-[3px] bg-success transition-[width] duration-300";
const GRID_STYLE = "mt-4 grid grid-cols-2 gap-4";

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
    <section className={cn("kmail-admin-page", PAGE_STYLE)}>
      <h2>Email Analytics</h2>
      <p className="mb-2 text-fg-muted">
        Activity dashboard for the authenticated user&rsquo;s mailbox.
        Tenant-wide rollups are a documented follow-up.
      </p>

      <div className="mb-4 flex items-center gap-4">
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

      {error && <p className="text-danger-fg">{error}</p>}

      {data && (
        <>
          {/* ── KPI cards ──────────────────────────────────── */}
          <div className="mb-4">
            <div className={CARD_STYLE}>
              <div className={NUM_STYLE}>{data.total_sent}</div>
              <div className={LABEL_STYLE}>Sent</div>
            </div>
            <div className={CARD_STYLE}>
              <div className={NUM_STYLE}>{data.total_received}</div>
              <div className={LABEL_STYLE}>Received</div>
            </div>
            <div className={CARD_STYLE}>
              <div className={NUM_STYLE}>
                {data.avg_response_seconds > 0
                  ? `${(data.avg_response_seconds / 3600).toFixed(1)}h`
                  : "—"}
              </div>
              <div className={LABEL_STYLE}>
                Avg response ({data.response_sample_size} threads)
              </div>
            </div>
            <div className={CARD_STYLE}>
              <div className={NUM_STYLE}>
                {data.range_start} — {data.range_end}
              </div>
              <div className={LABEL_STYLE}>Date range</div>
            </div>
          </div>

          {/* ── Daily chart ────────────────────────────────── */}
          <h3>Daily volume</h3>
          <table className={TABLE_STYLE}>
            <thead>
              <tr>
                <th className={TH_STYLE}>Date</th>
                <th className={TH_STYLE}>Sent</th>
                <th className={cn(TH_STYLE, "w-2/5")}>Chart (blue=sent, green=recv)</th>
                <th className={TH_STYLE}>Received</th>
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
                    <td className={TD_STYLE}>{d.date}</td>
                    <td className={TD_STYLE}>{d.sent}</td>
                    <td className={TD_STYLE}>
                      <div className="flex gap-0.5">
                        <div className={BAR_BASE} style={{ width: `${sentPct}%` }} />
                        <div className={BAR_RECV} style={{ width: `${recvPct}%` }} />
                      </div>
                    </td>
                    <td className={TD_STYLE}>{d.received}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>

          <div className={GRID_STYLE}>
            {/* ── Top recipients ──────────────────────────── */}
            <div>
              <h3>Top recipients</h3>
              <table className={TABLE_STYLE}>
                <thead>
                  <tr>
                    <th className={TH_STYLE}>#</th>
                    <th className={TH_STYLE}>Recipient</th>
                    <th className={TH_STYLE}>Emails</th>
                  </tr>
                </thead>
                <tbody>
                  {data.top_recipients.map((r, i) => (
                    <tr key={r.email}>
                      <td className={TD_STYLE}>{i + 1}</td>
                      <td className={TD_STYLE}>
                        {r.name ? `${r.name} <${r.email}>` : r.email}
                      </td>
                      <td className={TD_STYLE}>{r.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {/* ── Top senders ─────────────────────────────── */}
            <div>
              <h3>Top senders</h3>
              <table className={TABLE_STYLE}>
                <thead>
                  <tr>
                    <th className={TH_STYLE}>#</th>
                    <th className={TH_STYLE}>Sender</th>
                    <th className={TH_STYLE}>Emails</th>
                  </tr>
                </thead>
                <tbody>
                  {data.top_senders.map((s, i) => (
                    <tr key={s.email}>
                      <td className={TD_STYLE}>{i + 1}</td>
                      <td className={TD_STYLE}>
                        {s.name ? `${s.name} <${s.email}>` : s.email}
                      </td>
                      <td className={TD_STYLE}>{s.count}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>

          {/* ── Busiest hours ─────────────────────────────── */}
          <h3>Activity by hour (received messages)</h3>
          <div className="mt-2 flex h-[120px] items-end gap-0.5">
            {data.busiest_hours.map((h) => {
              const maxHour = Math.max(...data.busiest_hours.map((hh) => hh.count), 1);
              const pct = (h.count / maxHour) * 100;
              return (
                <div
                  key={h.hour}
                  title={`${h.hour}:00 — ${h.count} emails`}
                  className="flex-1 rounded-t-[3px] bg-info"
                  style={{
                    height: `${pct}%`,
                    minHeight: h.count > 0 ? 4 : 0,
                  }}
                />
              );
            })}
          </div>
          <div className="flex gap-0.5 text-[0.65rem] text-fg-muted">
            {data.busiest_hours.map((h) => (
              <div key={h.hour} className="flex-1 text-center">
                {h.hour}
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  );
}
