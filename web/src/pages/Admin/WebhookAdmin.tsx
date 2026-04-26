/**
 * Tenant outbound webhook management. Register, list, delete
 * endpoints and view recent delivery attempts.
 */

import { useCallback, useEffect, useState } from "react";

import {
  deleteWebhook,
  listWebhookDeliveries,
  listWebhooks,
  registerWebhook,
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
];

export default function WebhookAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [endpoints, setEndpoints] = useState<WebhookEndpoint[]>([]);
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([]);
  const [url, setUrl] = useState("");
  const [events, setEvents] = useState<string[]>([]);
  const [secret, setSecret] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    Promise.all([listWebhooks(tid), listWebhookDeliveries(tid, 100)])
      .then(([eps, dels]) => {
        setEndpoints(eps);
        setDeliveries(dels);
      })
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onRegister = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId) return;
    try {
      const out = await registerWebhook(selectedTenantId, url, events);
      setSecret(out.secret);
      setUrl("");
      setEvents([]);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDelete = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await deleteWebhook(selectedTenantId, id);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const toggleEvent = (ev: string) => {
    setEvents((prev) =>
      prev.includes(ev) ? prev.filter((e) => e !== ev) : [...prev, ev],
    );
  };

  return (
    <section className="kmail-admin-page">
      <h2>Webhooks</h2>
      <p className="kmail-admin-help">
        Subscribe a URL on your side to KMail events. Each delivery carries a{" "}
        <code>X-KMail-Signature: t=&lt;unix&gt;,v1=&lt;hex&gt;</code> header that
        you verify with HMAC-SHA256 against the body. Failed deliveries retry
        with exponential backoff up to three attempts.
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

      {selectedTenantId && (
        <>
          <form onSubmit={onRegister} style={{ display: "grid", gap: "0.5rem", maxWidth: 540 }}>
            <h3>Register webhook</h3>
            <label>
              URL
              <input type="url" required value={url} onChange={(e) => setUrl(e.target.value)} />
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
            <div className="kmail-admin-card">
              <h4>Signing secret (copy now — not shown again)</h4>
              <code style={{ wordBreak: "break-all" }}>{secret}</code>
            </div>
          )}

          <h3>Active endpoints</h3>
          <table className="kmail-admin-table">
            <thead>
              <tr>
                <th>URL</th>
                <th>Events</th>
                <th>Created</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {endpoints.map((ep) => (
                <tr key={ep.id}>
                  <td>{ep.url}</td>
                  <td>{ep.events.length === 0 ? "all" : ep.events.join(", ")}</td>
                  <td>{new Date(ep.created_at).toLocaleString()}</td>
                  <td>
                    <button onClick={() => onDelete(ep.id)}>Delete</button>
                  </td>
                </tr>
              ))}
              {endpoints.length === 0 && (
                <tr><td colSpan={4}>No endpoints registered.</td></tr>
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
              {deliveries.length === 0 && (
                <tr><td colSpan={5}>No deliveries yet.</td></tr>
              )}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}
