/**
 * Tenant outbound webhook management. Register, list, test-fire,
 * and revoke endpoints, view recent delivery attempts, and toggle
 * the signing scheme between v1 (legacy) and v2 (timestamp +
 * nonce replay protection).
 */

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  deleteWebhook,
  listWebhookDeliveries,
  listWebhooks,
  registerWebhook,
  testFireWebhook,
  updateWebhookSigningVersion,
  type WebhookDelivery,
  type WebhookEndpoint,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

const ALL_EVENTS = [
  "email.received",
  "email.bounced",
  "email.complaint",
  "calendar.event_created",
  "calendar.event_updated",
  "migration.completed",
  "domain.verified",
  "user.created",
];

type Health = "healthy" | "degraded" | "failing" | "idle";

function endpointHealth(deliveries: WebhookDelivery[], endpointId: string): Health {
  const recent = deliveries.filter((d) => d.endpoint_id === endpointId).slice(0, 10);
  if (recent.length === 0) return "idle";
  const failed = recent.filter((d) => d.status === "failed").length;
  if (failed === 0) return "healthy";
  if (failed >= recent.length / 2) return "failing";
  return "degraded";
}

export default function WebhookAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [endpoints, setEndpoints] = useState<WebhookEndpoint[]>([]);
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([]);
  const [url, setUrl] = useState("");
  const [events, setEvents] = useState<string[]>([]);
  const [signingVersion, setSigningVersion] = useState<"v1" | "v2">("v2");
  const [secret, setSecret] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<WebhookEndpoint | null>(null);

  const reload = useCallback((tid: string) => {
    setLoading(true);
    Promise.all([listWebhooks(tid), listWebhookDeliveries(tid, 100)])
      .then(([eps, dels]) => {
        setEndpoints(eps);
        setDeliveries(dels);
      })
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onRegister = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId) return;
    try {
      const out = await registerWebhook(selectedTenantId, url, events, signingVersion);
      setSecret(out.secret);
      setUrl("");
      setEvents([]);
      setInfo("Endpoint registered. Copy the signing secret now.");
      reload(selectedTenantId);
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const onTestFire = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await testFireWebhook(selectedTenantId, id);
      setInfo("Test ping enqueued.");
      reload(selectedTenantId);
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const onChangeSigningVersion = async (id: string, v: "v1" | "v2") => {
    if (!selectedTenantId) return;
    try {
      await updateWebhookSigningVersion(selectedTenantId, id, v);
      setInfo(`Signing version updated to ${v}.`);
      reload(selectedTenantId);
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const confirmDelete = (ep: WebhookEndpoint) => setPendingDelete(ep);
  const onDeleteConfirmed = async () => {
    if (!selectedTenantId || !pendingDelete) return;
    try {
      await deleteWebhook(selectedTenantId, pendingDelete.id);
      setInfo("Endpoint deleted.");
      setPendingDelete(null);
      reload(selectedTenantId);
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const toggleEvent = (ev: string) => {
    setEvents((prev) =>
      prev.includes(ev) ? prev.filter((e) => e !== ev) : [...prev, ev],
    );
  };

  const healthByEndpoint = useMemo(() => {
    const m = new Map<string, Health>();
    for (const ep of endpoints) m.set(ep.id, endpointHealth(deliveries, ep.id));
    return m;
  }, [endpoints, deliveries]);

  return (
    <section className="kmail-admin-page">
      <h2>Webhooks</h2>
      <p className="kmail-admin-help">
        Subscribe a URL on your side to KMail events. v1 signs with{" "}
        <code>X-KMail-Signature: t=&lt;unix&gt;,v1=&lt;hex&gt;</code>; v2 adds an{" "}
        <code>X-KMail-Webhook-Nonce</code> header and signs{" "}
        <code>timestamp.nonce.body</code> for replay protection. Failed
        deliveries retry with exponential backoff up to three attempts.
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

      {error && (
        <p className="kmail-error" role="alert">
          {error} <button onClick={() => setError(null)}>dismiss</button>
        </p>
      )}
      {info && (
        <p className="kmail-info" role="status">
          {info} <button onClick={() => setInfo(null)}>dismiss</button>
        </p>
      )}

      {selectedTenantId && (
        <>
          <form onSubmit={onRegister} style={{ display: "grid", gap: "0.5rem", maxWidth: 540 }}>
            <h3>Register webhook</h3>
            <label>
              URL
              <input type="url" required value={url} onChange={(e) => setUrl(e.target.value)} />
            </label>
            <label>
              Signing version
              <select
                value={signingVersion}
                onChange={(e) => setSigningVersion(e.target.value as "v1" | "v2")}
              >
                <option value="v2">v2 (recommended — replay-safe)</option>
                <option value="v1">v1 (legacy)</option>
              </select>
            </label>
            <fieldset>
              <legend>Events (none selected = all)</legend>
              {ALL_EVENTS.map((ev) => (
                <label key={ev} style={{ display: "block" }}>
                  <input
                    type="checkbox"
                    checked={events.includes(ev)}
                    onChange={() => toggleEvent(ev)}
                  />{" "}
                  {ev}
                </label>
              ))}
            </fieldset>
            <button type="submit">Register</button>
          </form>

          {secret && (
            <div className="kmail-admin-card" role="region" aria-label="Webhook secret">
              <h4>Signing secret (copy now — not shown again)</h4>
              <code style={{ wordBreak: "break-all", display: "block" }}>{secret}</code>
              <button
                onClick={() => {
                  navigator.clipboard?.writeText(secret).then(
                    () => setInfo("Secret copied."),
                    () => setError("Clipboard write failed."),
                  );
                }}
              >
                Copy secret
              </button>
              <button onClick={() => setSecret(null)}>Hide</button>
            </div>
          )}

          <h3>Active endpoints</h3>
          {loading && <p>Loading…</p>}
          <table className="kmail-admin-table">
            <thead>
              <tr>
                <th>URL</th>
                <th>Events</th>
                <th>Signing</th>
                <th>Health</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {endpoints.map((ep) => (
                <tr key={ep.id}>
                  <td><code>{ep.url}</code></td>
                  <td>{ep.events.length === 0 ? "all" : ep.events.join(", ")}</td>
                  <td>
                    <select
                      value={ep.signing_version ?? "v1"}
                      onChange={(e) =>
                        onChangeSigningVersion(ep.id, e.target.value as "v1" | "v2")
                      }
                    >
                      <option value="v1">v1</option>
                      <option value="v2">v2</option>
                    </select>
                  </td>
                  <td>
                    <HealthBadge health={healthByEndpoint.get(ep.id) ?? "idle"} />
                  </td>
                  <td>{new Date(ep.created_at).toLocaleString()}</td>
                  <td>
                    <button onClick={() => onTestFire(ep.id)}>Test fire</button>{" "}
                    <button onClick={() => confirmDelete(ep)}>Delete</button>
                  </td>
                </tr>
              ))}
              {!loading && endpoints.length === 0 && (
                <tr><td colSpan={6}>No endpoints registered.</td></tr>
              )}
            </tbody>
          </table>

          <h3>Recent deliveries</h3>
          <table className="kmail-admin-table">
            <thead>
              <tr>
                <th>Event</th>
                <th>Status</th>
                <th>Attempts</th>
                <th>Last error</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {deliveries.map((d) => (
                <tr key={d.id}>
                  <td>{d.event_type}</td>
                  <td>{d.status}{d.last_status ? ` (${d.last_status})` : ""}</td>
                  <td>{d.attempts}</td>
                  <td>{d.last_error ?? ""}</td>
                  <td>{new Date(d.created_at).toLocaleString()}</td>
                </tr>
              ))}
              {!loading && deliveries.length === 0 && (
                <tr><td colSpan={5}>No deliveries yet.</td></tr>
              )}
            </tbody>
          </table>
        </>
      )}

      {pendingDelete && (
        <div role="dialog" aria-modal="true" className="kmail-modal">
          <div className="kmail-modal-body">
            <p>
              Delete webhook <code>{pendingDelete.url}</code>? Pending deliveries
              will be cancelled.
            </p>
            <div style={{ display: "flex", gap: "0.5rem" }}>
              <button onClick={onDeleteConfirmed}>Delete</button>
              <button onClick={() => setPendingDelete(null)}>Cancel</button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

function HealthBadge({ health }: { health: Health }) {
  const colors: Record<Health, string> = {
    healthy: "#16a34a",
    degraded: "#d97706",
    failing: "#dc2626",
    idle: "#6b7280",
  };
  return (
    <span
      style={{
        display: "inline-block",
        padding: "0 0.5rem",
        borderRadius: 8,
        background: colors[health],
        color: "white",
        fontSize: 12,
      }}
    >
      {health}
    </span>
  );
}
