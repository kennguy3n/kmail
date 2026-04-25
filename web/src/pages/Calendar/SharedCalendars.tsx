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
      {error && <p className="kmail-error">{error}</p>}

      <h3>Shared with me</h3>
      <table className="kmail-admin-table">
        <thead>
          <tr>
            <th>Calendar</th>
            <th>Owner</th>
            <th>Permission</th>
            <th>Since</th>
          </tr>
        </thead>
        <tbody>
          {shares.map((s) => (
            <tr key={s.id}>
              <td>{s.calendar_id}</td>
              <td>{s.owner_account_id}</td>
              <td>{s.permission}</td>
              <td>{new Date(s.created_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>

      <h3>Grant a share</h3>
      <form onSubmit={submit}>
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
        <button type="submit">Share</button>
      </form>
    </section>
  );
}
