import { useEffect, useState } from "react";

import {
  listSharedCalendars,
  shareCalendar,
  type CalendarShare,
} from "../../api/calendarSharing";

/**
 * SharedCalendars lists every calendar shared with the current
 * principal and lets the owner grant new shares.
 */
export default function SharedCalendars() {
  const [shares, setShares] = useState<CalendarShare[]>([]);
  const [calendarId, setCalendarId] = useState("");
  const [target, setTarget] = useState("");
  const [permission, setPermission] = useState<CalendarShare["permission"]>("read");
  const [error, setError] = useState<string | null>(null);

  const reload = async () => {
    try {
      const rows = await listSharedCalendars();
      setShares(rows);
    } catch (e) {
      setError(String(e));
    }
  };

  useEffect(() => {
    void reload();
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      await shareCalendar(calendarId, target, permission);
      setCalendarId("");
      setTarget("");
      setPermission("read");
      await reload();
    } catch (err) {
      setError(String(err));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Shared calendars</h2>
      <p className="kmail-admin-hint">Manage calendars shared with you and grant access to others.</p>
      {error && <p className="kmail-error">{error}</p>}

      <h3>Shared with me</h3>
      {shares.length === 0 && <p className="kmail-admin-hint">No calendars shared with you yet.</p>}
      {shares.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {shares.map((s) => (
            <div
              key={s.id}
              className="flex flex-col gap-2 rounded-xl border border-border bg-surface p-4 shadow-sm"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="font-semibold text-fg">
                  {s.calendar_name || s.calendar_id}
                </div>
                <span
                  className={
                    s.permission === "admin"
                      ? "kmail-flag-pending"
                      : s.permission === "readwrite"
                        ? "kmail-flag-ok"
                        : "inline-flex rounded-pill bg-info-bg px-2 py-0.5 text-xs font-semibold text-info-fg"
                  }
                >
                  {s.permission}
                </span>
              </div>
              <div className="text-sm text-fg-muted">
                Owner: {s.owner_name || s.owner_account_id}
              </div>
              <div className="text-xs text-fg-subtle">
                Shared with {s.target_name || s.target_account_id} · {new Date(s.created_at).toLocaleDateString()}
              </div>
            </div>
          ))}
        </div>
      )}

      <h3 className="mt-4">Grant a share</h3>
      <form onSubmit={submit} className="kmail-admin-card">
        <div className="kmail-admin-controls">
          <label>
            Calendar ID
            <input value={calendarId} onChange={(e) => setCalendarId(e.target.value)} required />
          </label>
          <label>
            Target account
            <input value={target} onChange={(e) => setTarget(e.target.value)} required />
          </label>
          <label>
            Permission
            <select value={permission} onChange={(e) => setPermission(e.target.value as CalendarShare["permission"])}>
              <option value="read">read</option>
              <option value="readwrite">readwrite</option>
              <option value="admin">admin</option>
            </select>
          </label>
        </div>
        <button type="submit" className="kmail-button">Share</button>
      </form>
    </section>
  );
}
