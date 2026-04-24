import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import type {
  Calendar,
  CalendarEvent,
  CalendarEventDraft,
  EventParticipant,
} from "../../types";

/**
 * EventCreate is the event creation / editing form.
 *
 * Two modes:
 *   - Create: `/calendar/new` (optionally seeded by `?start=` and
 *     `?end=` query params from the slot-click handler on
 *     CalendarView).
 *   - Edit: `/calendar/:eventId/edit`. The form pre-populates from
 *     `CalendarEvent/get` and submits an `updateEvent()` call.
 *
 * On success the user is navigated back to the calendar view. In
 * Phase 3 the Calendar Bridge will also emit a chat message into
 * the meeting's backing channel when `rsvpRequired` is set; the
 * BFF swallows that wire-up today so the UI stays unchanged.
 */
export default function EventCreate() {
  const navigate = useNavigate();
  const { eventId } = useParams<{ eventId?: string }>();
  const [searchParams] = useSearchParams();
  const isEdit = !!eventId;

  const [calendars, setCalendars] = useState<Calendar[] | null>(null);
  const [calendarId, setCalendarId] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [location, setLocation] = useState("");
  const [startLocal, setStartLocal] = useState(
    seedDatetimeLocal(searchParams.get("start")) ?? defaultStart(),
  );
  const [endLocal, setEndLocal] = useState(
    seedDatetimeLocal(searchParams.get("end")) ?? defaultEnd(),
  );
  const [participantsRaw, setParticipantsRaw] = useState("");
  const [rsvpRequired, setRsvpRequired] = useState(true);
  const [status, setStatus] = useState<
    "confirmed" | "tentative" | "cancelled"
  >("confirmed");
  const [isSubmitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loadingEvent, setLoadingEvent] = useState(isEdit);
  const [loadingCalendars, setLoadingCalendars] = useState(true);
  // The event we loaded in edit mode, kept so the submit handler
  // can diff the form against it and ship only changed fields to
  // `updateEvent()` (which rejects no-op updates server-side).
  const originalEventRef = useRef<CalendarEvent | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoadingCalendars(true);
    jmapClient
      .getCalendars()
      .then((list) => {
        if (cancelled) return;
        setCalendars(list);
        if (!isEdit) {
          const def = list.find((c) => c.isDefault) ?? list[0];
          if (def) setCalendarId((current) => current || def.id);
        }
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoadingCalendars(false);
      });
    return () => {
      cancelled = true;
    };
  }, [isEdit]);

  useEffect(() => {
    if (!isEdit || !eventId) return;
    let cancelled = false;
    setLoadingEvent(true);
    jmapClient
      .getEvent(eventId)
      .then((e: CalendarEvent) => {
        if (cancelled) return;
        originalEventRef.current = e;
        setCalendarId(e.calendarId);
        setTitle(e.title);
        setDescription(e.description ?? "");
        setLocation(e.location ?? "");
        setStartLocal(toDatetimeLocal(new Date(e.start)));
        setEndLocal(toDatetimeLocal(new Date(e.end)));
        setParticipantsRaw(
          (e.participants ?? [])
            .map((p) => (p.name ? `${p.name} <${p.email}>` : p.email))
            .join(", "),
        );
        if (e.status) setStatus(e.status);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoadingEvent(false);
      });
    return () => {
      cancelled = true;
    };
  }, [eventId, isEdit]);

  const canSubmit = useMemo(() => {
    if (isSubmitting) return false;
    if (!calendarId) return false;
    if (!title.trim()) return false;
    if (!startLocal || !endLocal) return false;
    const start = new Date(startLocal);
    const end = new Date(endLocal);
    if (
      Number.isNaN(start.getTime()) ||
      Number.isNaN(end.getTime()) ||
      end <= start
    ) {
      return false;
    }
    return true;
  }, [calendarId, endLocal, isSubmitting, startLocal, title]);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (!canSubmit) return;
    const participants = parseParticipants(participantsRaw, rsvpRequired);
    const startIso = new Date(startLocal).toISOString();
    const endIso = new Date(endLocal).toISOString();
    const draft: CalendarEventDraft = {
      calendarId,
      title: title.trim(),
      description: description.trim() || undefined,
      location: location.trim() || undefined,
      start: startIso,
      end: endIso,
      participants: participants.length > 0 ? participants : undefined,
      status,
    };
    setSubmitting(true);
    try {
      if (isEdit && eventId) {
        const original = originalEventRef.current;
        const changes = original
          ? diffEventDraft(original, draft)
          : draft;
        if (Object.keys(changes).length === 0) {
          navigate("/calendar");
          return;
        }
        await jmapClient.updateEvent(eventId, changes);
      } else {
        await jmapClient.createEvent(draft);
      }
      navigate("/calendar");
    } catch (err: unknown) {
      setError(errorMessage(err));
      setSubmitting(false);
    }
  };

  const heading = isEdit ? "Edit event" : "New event";

  return (
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>{heading}</h2>
      </header>
      {error && (
        <div style={styles.error} role="alert">
          <span>{error}</span>
          <button
            type="button"
            onClick={() => setError(null)}
            style={styles.errorDismiss}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {(loadingEvent || loadingCalendars) && (
        <p style={styles.muted}>Loading…</p>
      )}
      <form onSubmit={handleSubmit} style={styles.form}>
        <div style={styles.row}>
          <label htmlFor="event-calendar" style={styles.label}>
            Calendar
          </label>
          <select
            id="event-calendar"
            value={calendarId}
            onChange={(e) => setCalendarId(e.target.value)}
            style={styles.select}
            disabled={!calendars || calendars.length === 0}
            required
          >
            {(calendars ?? []).length === 0 ? (
              <option value="">(loading calendars…)</option>
            ) : (
              (calendars ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                  {c.isDefault ? " (default)" : ""}
                </option>
              ))
            )}
          </select>
        </div>
        <div style={styles.row}>
          <label htmlFor="event-title" style={styles.label}>
            Title
          </label>
          <input
            id="event-title"
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            style={styles.input}
            required
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="event-start" style={styles.label}>
            Start
          </label>
          <input
            id="event-start"
            type="datetime-local"
            value={startLocal}
            onChange={(e) => setStartLocal(e.target.value)}
            style={styles.input}
            required
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="event-end" style={styles.label}>
            End
          </label>
          <input
            id="event-end"
            type="datetime-local"
            value={endLocal}
            onChange={(e) => setEndLocal(e.target.value)}
            style={styles.input}
            required
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="event-location" style={styles.label}>
            Location
          </label>
          <input
            id="event-location"
            type="text"
            value={location}
            onChange={(e) => setLocation(e.target.value)}
            style={styles.input}
            placeholder="Room, URL, or address"
          />
        </div>
        <div style={styles.row}>
          <label htmlFor="event-participants" style={styles.label}>
            Participants
          </label>
          <input
            id="event-participants"
            type="text"
            value={participantsRaw}
            onChange={(e) => setParticipantsRaw(e.target.value)}
            style={styles.input}
            placeholder="name@example.com, Other Person <other@example.com>"
          />
        </div>
        <div style={styles.row}>
          <label style={styles.label}>RSVP</label>
          <label style={styles.inlineCheckbox}>
            <input
              type="checkbox"
              checked={rsvpRequired}
              onChange={(e) => setRsvpRequired(e.target.checked)}
            />
            Require RSVP from participants
          </label>
        </div>
        <div style={styles.row}>
          <label htmlFor="event-status" style={styles.label}>
            Status
          </label>
          <select
            id="event-status"
            value={status}
            onChange={(e) =>
              setStatus(e.target.value as typeof status)
            }
            style={styles.select}
          >
            <option value="confirmed">Confirmed</option>
            <option value="tentative">Tentative</option>
            <option value="cancelled">Cancelled</option>
          </select>
        </div>
        <div style={styles.bodyRow}>
          <label htmlFor="event-description" style={styles.label}>
            Description
          </label>
          <textarea
            id="event-description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            style={styles.textarea}
            rows={6}
          />
        </div>
        <div style={styles.buttonRow}>
          <button
            type="submit"
            disabled={!canSubmit}
            style={{
              ...styles.primaryButton,
              opacity: canSubmit ? 1 : 0.6,
              cursor: canSubmit ? "pointer" : "not-allowed",
            }}
          >
            {isSubmitting
              ? isEdit
                ? "Saving…"
                : "Creating…"
              : isEdit
                ? "Save"
                : "Create event"}
          </button>
          <button
            type="button"
            onClick={() => navigate(-1)}
            style={styles.secondaryButton}
            disabled={isSubmitting}
          >
            Cancel
          </button>
        </div>
      </form>
    </section>
  );
}

/**
 * Build a `Partial<CalendarEventDraft>` containing only the
 * fields whose current form value differs from `original`. The
 * BFF's `CalendarEvent/set update` rejects no-op updates, so the
 * submit handler must strip unchanged fields before calling
 * `updateEvent()`. Participants are compared by their normalised
 * `(email, name, role, rsvp)` tuple sorted by email so reordering
 * alone is not treated as a change.
 */
function diffEventDraft(
  original: CalendarEvent,
  draft: CalendarEventDraft,
): Partial<CalendarEventDraft> {
  const changes: Partial<CalendarEventDraft> = {};
  if (draft.calendarId !== original.calendarId) {
    changes.calendarId = draft.calendarId;
  }
  if (draft.title !== original.title) {
    changes.title = draft.title;
  }
  if ((draft.description ?? "") !== (original.description ?? "")) {
    changes.description = draft.description;
  }
  if ((draft.location ?? "") !== (original.location ?? "")) {
    changes.location = draft.location;
  }
  if (!sameInstant(draft.start, original.start)) {
    changes.start = draft.start;
  }
  if (!sameInstant(draft.end, original.end)) {
    changes.end = draft.end;
  }
  if ((draft.status ?? undefined) !== (original.status ?? undefined)) {
    changes.status = draft.status;
  }
  if (
    !sameParticipants(
      draft.participants ?? [],
      original.participants ?? [],
    )
  ) {
    changes.participants = draft.participants;
  }
  return changes;
}

function sameInstant(a: string, b: string): boolean {
  const ta = Date.parse(a);
  const tb = Date.parse(b);
  if (Number.isNaN(ta) || Number.isNaN(tb)) return a === b;
  return ta === tb;
}

function sameParticipants(
  a: EventParticipant[],
  b: EventParticipant[],
): boolean {
  if (a.length !== b.length) return false;
  const norm = (ps: EventParticipant[]) =>
    ps
      .map((p) =>
        [p.email, p.name ?? "", p.role ?? "", p.rsvp ?? ""].join("\u0000"),
      )
      .sort();
  const na = norm(a);
  const nb = norm(b);
  for (let i = 0; i < na.length; i++) {
    if (na[i] !== nb[i]) return false;
  }
  return true;
}

function parseParticipants(
  raw: string,
  rsvpRequired: boolean,
): EventParticipant[] {
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0)
    .map((s): EventParticipant => {
      const match = s.match(/^(.*)<\s*([^>]+)\s*>\s*$/);
      const participant: EventParticipant = match
        ? {
            name: match[1].trim() || null,
            email: match[2].trim(),
            role: "required",
          }
        : { email: s, role: "required" };
      if (rsvpRequired) participant.rsvp = "needs-action";
      return participant;
    });
}

function toDatetimeLocal(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function seedDatetimeLocal(iso: string | null): string | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  return toDatetimeLocal(d);
}

function defaultStart(): string {
  const d = new Date();
  d.setMinutes(0, 0, 0);
  d.setHours(d.getHours() + 1);
  return toDatetimeLocal(d);
}

function defaultEnd(): string {
  const d = new Date();
  d.setMinutes(0, 0, 0);
  d.setHours(d.getHours() + 2);
  return toDatetimeLocal(d);
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Unknown error";
}

const styles: Record<string, React.CSSProperties> = {
  root: {
    padding: "1rem",
    maxWidth: "760px",
  },
  header: {
    marginBottom: "0.75rem",
  },
  title: {
    margin: 0,
    fontSize: "1.25rem",
  },
  error: {
    padding: "0.5rem 0.75rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    marginBottom: "0.75rem",
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: "0.5rem",
  },
  errorDismiss: {
    background: "transparent",
    border: "none",
    color: "#991b1b",
    fontSize: "1.1rem",
    cursor: "pointer",
    lineHeight: 1,
    padding: "0 0.25rem",
  },
  muted: {
    color: "#6b7280",
    fontStyle: "italic",
  },
  form: {
    display: "flex",
    flexDirection: "column",
    gap: "0.5rem",
    border: "1px solid #e5e7eb",
    borderRadius: "0.5rem",
    padding: "1rem",
    background: "#fff",
  },
  row: {
    display: "grid",
    gridTemplateColumns: "120px 1fr",
    alignItems: "center",
    gap: "0.5rem",
  },
  bodyRow: {
    display: "grid",
    gridTemplateColumns: "120px 1fr",
    alignItems: "start",
    gap: "0.5rem",
  },
  label: {
    fontSize: "0.85rem",
    color: "#374151",
    fontWeight: 600,
  },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  select: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    background: "#fff",
  },
  textarea: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    resize: "vertical",
    minHeight: "6rem",
    font: "inherit",
  },
  inlineCheckbox: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.9rem",
  },
  buttonRow: {
    display: "flex",
    gap: "0.5rem",
    marginTop: "0.5rem",
  },
  primaryButton: {
    padding: "0.4rem 1rem",
    fontSize: "0.9rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
  },
  secondaryButton: {
    padding: "0.4rem 1rem",
    fontSize: "0.9rem",
    background: "#fff",
    color: "#374151",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
};
