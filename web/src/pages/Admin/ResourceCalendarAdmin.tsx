import { useEffect, useState } from "react";

import {
  bookResource,
  createResourceCalendar,
  listResourceCalendars,
  type ResourceCalendar,
} from "../../api/calendarSharing";

/**
 * ResourceCalendarAdmin manages the bookable-resource registry
 * (rooms, equipment, vehicles) and exposes a test-book form.
 */
export default function ResourceCalendarAdmin() {
  const [rows, setRows] = useState<ResourceCalendar[]>([]);
  const [draft, setDraft] = useState<Partial<ResourceCalendar>>({ resource_type: "room" });
  const [error, setError] = useState<string | null>(null);
  const [booking, setBooking] = useState<{
    id: string;
    start: string;
    end: string;
    subject: string;
  } | null>(null);

  const reload = () => listResourceCalendars().then(setRows).catch((e) => setError(String(e)));

  useEffect(() => {
    void reload();
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      await createResourceCalendar(draft);
      setDraft({ resource_type: "room" });
      await reload();
    } catch (err) {
      setError(String(err));
    }
  };

  const doBook = async () => {
    if (!booking) return;
    try {
      await bookResource(booking.id, booking.start, booking.end, booking.subject);
      setBooking(null);
    } catch (err) {
      setError(String(err));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Resource calendars</h2>
      {error && <p className="kmail-error">{error}</p>}

      <table className="kmail-admin-table">
        <thead>
          <tr><th>Name</th><th>Type</th><th>Location</th><th>Capacity</th><th></th></tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id}>
              <td>{r.name}</td>
              <td>{r.resource_type}</td>
              <td>{r.location}</td>
              <td>{r.capacity}</td>
              <td>
                <button
                  type="button"
                  onClick={() =>
                    setBooking({
                      // BookResource hits CalDAV via calendarPath()
                      // and needs the collection slug, not the DB
                      // UUID. Fall back to id when caldav_id is empty
                      // (e.g. fixtures created before the column
                      // existed).
                      id: r.caldav_id || r.id,
                      start: new Date().toISOString(),
                      end: new Date(Date.now() + 3600_000).toISOString(),
                      subject: `Booking ${r.name}`,
                    })
                  }
                >
                  Book
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <h3>Register a resource</h3>
      <form onSubmit={submit}>
        <label>
          Name
          <input value={draft.name ?? ""} onChange={(e) => setDraft({ ...draft, name: e.target.value })} required />
        </label>
        <label>
          Type
          <select
            value={draft.resource_type ?? "room"}
            onChange={(e) =>
              setDraft({ ...draft, resource_type: e.target.value as ResourceCalendar["resource_type"] })
            }
          >
            <option value="room">Room</option>
            <option value="equipment">Equipment</option>
            <option value="vehicle">Vehicle</option>
          </select>
        </label>
        <label>
          Location
          <input value={draft.location ?? ""} onChange={(e) => setDraft({ ...draft, location: e.target.value })} />
        </label>
        <label>
          Capacity
          <input
            type="number"
            value={draft.capacity ?? 0}
            onChange={(e) => setDraft({ ...draft, capacity: Number(e.target.value) })}
          />
        </label>
        <button type="submit">Create</button>
      </form>

      <CalendarNotificationSettings rows={rows} />

      {booking && (
        <div className="kmail-booking-dialog">
          <h3>Book {booking.id}</h3>
          <label>
            Subject
            <input value={booking.subject} onChange={(e) => setBooking({ ...booking, subject: e.target.value })} />
          </label>
          <label>
            Start
            <input value={booking.start} onChange={(e) => setBooking({ ...booking, start: e.target.value })} />
          </label>
          <label>
            End
            <input value={booking.end} onChange={(e) => setBooking({ ...booking, end: e.target.value })} />
          </label>
          <button type="button" onClick={doBook}>Confirm</button>
          <button type="button" onClick={() => setBooking(null)}>Cancel</button>
        </div>
      )}
    </section>
  );
}

/**
 * CalendarNotificationSettings — Phase 5 closeout: lets a tenant
 * admin pick a default KChat channel for calendar notifications,
 * and override the channel on a per-resource basis.
 */
function CalendarNotificationSettings({ rows }: { rows: ResourceCalendar[] }) {
  const [tenantId, setTenantId] = useState<string>("");
  const [defaultChannel, setDefaultChannel] = useState<string>("");
  const [perCalendarChannel, setPerCalendarChannel] = useState<Record<string, string>>({});
  const [status, setStatus] = useState<string | null>(null);

  return (
    <section style={{ marginTop: "2rem" }}>
      <h3>Calendar notification routing</h3>
      <p>
        Pick the KChat channel events publish to. Per-resource overrides
        take priority over the tenant default; if neither is set, the
        environment-default channel applies.
      </p>

      <label>
        Tenant ID
        <input value={tenantId} onChange={(e) => setTenantId(e.target.value)} />
      </label>
      <label>
        Default channel ID
        <input value={defaultChannel} onChange={(e) => setDefaultChannel(e.target.value)} />
      </label>
      <button
        onClick={async () => {
          if (!tenantId || !defaultChannel) return;
          const { setCalendarDefaultChannel } = await import("../../api/admin");
          try {
            await setCalendarDefaultChannel(tenantId, defaultChannel);
            setStatus("Saved tenant default.");
          } catch (e: unknown) {
            setStatus(String(e));
          }
        }}
      >
        Save default
      </button>

      <h4>Per-resource channels</h4>
      <table className="kmail-admin-table">
        <thead>
          <tr><th>Resource</th><th>Channel ID</th><th></th></tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id}>
              <td>{r.name ?? r.id}</td>
              <td>
                <input
                  value={perCalendarChannel[r.id] ?? ""}
                  onChange={(e) => setPerCalendarChannel((prev) => ({ ...prev, [r.id]: e.target.value }))}
                />
              </td>
              <td>
                <button
                  onClick={async () => {
                    if (!tenantId) return;
                    const channelId = perCalendarChannel[r.id] ?? "";
                    if (!channelId) return;
                    const { setCalendarChannelMapping } = await import("../../api/admin");
                    try {
                      await setCalendarChannelMapping(tenantId, r.id, channelId);
                      setStatus(`Saved override for ${r.name ?? r.id}.`);
                    } catch (e: unknown) {
                      setStatus(String(e));
                    }
                  }}
                >
                  Save
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {status && <p>{status}</p>}
    </section>
  );
}
