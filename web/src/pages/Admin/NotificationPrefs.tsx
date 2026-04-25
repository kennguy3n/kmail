import { useEffect, useState } from "react";

import {
  getPreferences,
  updatePreferences,
  listSubscriptions,
  unsubscribe,
  type NotificationPreference,
  type PushSubscription,
} from "../../api/push";

/**
 * NotificationPrefs exposes the per-user push preferences + the
 * list of registered devices.
 */
export default function NotificationPrefs() {
  const [prefs, setPrefs] = useState<NotificationPreference | null>(null);
  const [subs, setSubs] = useState<PushSubscription[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = async () => {
    try {
      const [p, s] = await Promise.all([getPreferences(), listSubscriptions()]);
      setPrefs(p);
      setSubs(s);
    } catch (e) {
      setError(String(e));
    }
  };

  useEffect(() => {
    void reload();
  }, []);

  const save = async () => {
    if (!prefs) return;
    setSaving(true);
    try {
      const out = await updatePreferences(prefs);
      setPrefs(out);
    } catch (e) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };

  const removeSub = async (id: string) => {
    try {
      await unsubscribe(id);
      await reload();
    } catch (e) {
      setError(String(e));
    }
  };

  if (!prefs) return <section className="kmail-admin-page"><p>Loading…</p></section>;

  return (
    <section className="kmail-admin-page">
      <h2>Notification preferences</h2>
      {error && <p className="kmail-error">{error}</p>}

      <form
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <label>
          <input
            type="checkbox"
            checked={prefs.new_email}
            onChange={(e) => setPrefs({ ...prefs, new_email: e.target.checked })}
          />
          New email
        </label>
        <label>
          <input
            type="checkbox"
            checked={prefs.calendar_reminder}
            onChange={(e) => setPrefs({ ...prefs, calendar_reminder: e.target.checked })}
          />
          Calendar reminders
        </label>
        <label>
          <input
            type="checkbox"
            checked={prefs.shared_inbox}
            onChange={(e) => setPrefs({ ...prefs, shared_inbox: e.target.checked })}
          />
          Shared inbox activity
        </label>
        <label>
          Quiet hours start (HH:MM)
          <input
            value={prefs.quiet_hours_start}
            onChange={(e) => setPrefs({ ...prefs, quiet_hours_start: e.target.value })}
          />
        </label>
        <label>
          Quiet hours end (HH:MM)
          <input
            value={prefs.quiet_hours_end}
            onChange={(e) => setPrefs({ ...prefs, quiet_hours_end: e.target.value })}
          />
        </label>
        <button type="submit" disabled={saving}>
          {saving ? "Saving…" : "Save"}
        </button>
      </form>

      <h3>Registered devices</h3>
      {subs.length === 0 ? (
        <p>No devices registered.</p>
      ) : (
        <table className="kmail-admin-table">
          <thead>
            <tr>
              <th>Device</th>
              <th>Endpoint</th>
              <th>Added</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {subs.map((s) => (
              <tr key={s.id}>
                <td>{s.device_type}</td>
                <td><code>{s.push_endpoint.slice(0, 48)}…</code></td>
                <td>{new Date(s.created_at).toLocaleString()}</td>
                <td><button type="button" onClick={() => removeSub(s.id)}>Remove</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
