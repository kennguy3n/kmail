import { useCallback, useEffect, useState } from "react";

import {
  listIpReputation,
  getIpReputationHistory,
  type IPReputationMetrics,
  type IPReputationHistoryPoint,
} from "../../api/admin";

/**
 * IpReputationAdmin renders the global IP-reputation dashboard:
 * per-pool rollups, per-IP metrics, and an expandable 30-day trend.
 */
export default function IpReputationAdmin() {
  const [ips, setIps] = useState<IPReputationMetrics[]>([]);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [history, setHistory] = useState<Record<string, IPReputationHistoryPoint[]>>({});
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(() => {
    listIpReputation().then(setIps).catch((e) => setError(String(e)));
  }, []);

  useEffect(() => {
    reload();
  }, [reload]);

  const toggle = async (ipId: string) => {
    if (expanded === ipId) {
      setExpanded(null);
      return;
    }
    setExpanded(ipId);
    if (!history[ipId]) {
      try {
        const h = await getIpReputationHistory(ipId);
        setHistory((prev) => ({ ...prev, [ipId]: h }));
      } catch (e) {
        setError(String(e));
      }
    }
  };

  const poolGroups = ips.reduce<Record<string, IPReputationMetrics[]>>((acc, ip) => {
    (acc[ip.pool_name] ??= []).push(ip);
    return acc;
  }, {});

  const color = (score: number): string => {
    if (score >= 80) return "rep-green";
    if (score >= 50) return "rep-yellow";
    return "rep-red";
  };

  return (
    <section className="kmail-admin-page">
      <h2>IP Reputation</h2>
      {error && <p className="kmail-error">{error}</p>}

      <h3>Pool overview</h3>
      <table className="kmail-admin-table">
        <thead>
          <tr>
            <th>Pool</th>
            <th>Type</th>
            <th>IPs</th>
            <th>Avg reputation</th>
            <th>Daily volume</th>
          </tr>
        </thead>
        <tbody>
          {Object.entries(poolGroups).map(([name, members]) => {
            const avg = members.reduce((s, ip) => s + ip.reputation_score, 0) / members.length;
            const vol = members.reduce((s, ip) => s + ip.daily_volume, 0);
            return (
              <tr key={name}>
                <td>{name}</td>
                <td>{members[0]?.pool_type}</td>
                <td>{members.length}</td>
                <td className={color(avg)}>{avg.toFixed(1)}</td>
                <td>{vol.toLocaleString()}</td>
              </tr>
            );
          })}
        </tbody>
      </table>

      <h3>Per-IP metrics</h3>
      <table className="kmail-admin-table">
        <thead>
          <tr>
            <th>Address</th>
            <th>Pool</th>
            <th>Reputation</th>
            <th>Daily volume</th>
            <th>Status</th>
            <th>Warmup day</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {ips.map((ip) => (
            <>
              <tr key={ip.ip_id}>
                <td><code>{ip.address}</code></td>
                <td>{ip.pool_name}</td>
                <td className={color(ip.reputation_score)}>{ip.reputation_score}</td>
                <td>{ip.daily_volume.toLocaleString()}</td>
                <td>{ip.status}</td>
                <td>{ip.warmup_day}</td>
                <td>
                  <button type="button" onClick={() => toggle(ip.ip_id)}>
                    {expanded === ip.ip_id ? "Hide" : "Trend"}
                  </button>
                </td>
              </tr>
              {expanded === ip.ip_id && (
                <tr>
                  <td colSpan={7}>
                    {history[ip.ip_id] ? (
                      <table className="kmail-admin-table">
                        <thead>
                          <tr><th>Day</th><th>Reputation</th><th>Volume</th></tr>
                        </thead>
                        <tbody>
                          {history[ip.ip_id].map((p) => (
                            <tr key={p.day}>
                              <td>{new Date(p.day).toLocaleDateString()}</td>
                              <td>{p.reputation_score}</td>
                              <td>{p.daily_volume}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    ) : (
                      <p>Loading trend…</p>
                    )}
                  </td>
                </tr>
              )}
            </>
          ))}
        </tbody>
      </table>
    </section>
  );
}
