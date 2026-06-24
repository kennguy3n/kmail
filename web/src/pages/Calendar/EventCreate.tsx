import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";

import { cn } from "../../lib/cn";

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
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>{heading}</h2>
      </header>
      {error && (
        <div className={styles.error} role="alert">
          <span>{error}</span>
          <button
            type="button"
            onClick={() => setError(null)}
            className={styles.errorDismiss}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {(loadingEvent || loadingCalendars) && (
        <p className={styles.muted}>Loading…</p>
      )}
      <form onSubmit={handleSubmit} className={styles.form}>
        <div className={styles.row}>
          <label htmlFor="event-calendar" className={styles.label}>
            Calendar
          </label>
          <select
            id="event-calendar"
            value={calendarId}
            onChange={(e) => setCalendarId(e.target.value)}
            className={styles.select}
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
        <div className={styles.row}>
          <label htmlFor="event-title" className={styles.label}>
            Title
          </label>
          <input
            id="event-title"
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            className={styles.input}
            required
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="event-start" className={styles.label}>
            Start
          </label>
          <input
            id="event-start"
            type="datetime-local"
            value={startLocal}
            onChange={(e) => setStartLocal(e.target.value)}
            className={styles.input}
            required
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="event-end" className={styles.label}>
            End
          </label>
          <input
            id="event-end"
            type="datetime-local"
            value={endLocal}
            onChange={(e) => setEndLocal(e.target.value)}
            className={styles.input}
            required
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="event-location" className={styles.label}>
            Location
          </label>
          <input
            id="event-location"
            type="text"
            value={location}
            onChange={(e) => setLocation(e.target.value)}
            className={styles.input}
            placeholder="Room, URL, or address"
          />
        </div>
        <div className={styles.row}>
          <label htmlFor="event-participants" className={styles.label}>
            Participants
          </label>
          <input
            id="event-participants"
            type="text"
            value={participantsRaw}
            onChange={(e) => setParticipantsRaw(e.target.value)}
            className={styles.input}
            placeholder="name@example.com, Other Person <other@example.com>"
          />
        </div>
        <FreeBusyCheck
          calendarId={calendarId}
          startLocal={startLocal}
          endLocal={endLocal}
        />
        <div className={styles.row}>
          <label className={styles.label}>RSVP</label>
          <label className={styles.inlineCheckbox}>
            <input
              type="checkbox"
              checked={rsvpRequired}
              onChange={(e) => setRsvpRequired(e.target.checked)}
            />
            Require RSVP from participants
          </label>
        </div>
        <div className={styles.row}>
          <label htmlFor="event-status" className={styles.label}>
            Status
          </label>
          <select
            id="event-status"
            value={status}
            onChange={(e) =>
              setStatus(e.target.value as typeof status)
            }
            className={styles.select}
          >
            <option value="confirmed">Confirmed</option>
            <option value="tentative">Tentative</option>
            <option value="cancelled">Cancelled</option>
          </select>
        </div>
        <div className={styles.bodyRow}>
          <label htmlFor="event-description" className={styles.label}>
            Description
          </label>
          <textarea
            id="event-description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className={styles.textarea}
            rows={6}
          />
        </div>
        <div className={styles.buttonRow}>
          <button
            type="submit"
            disabled={!canSubmit}
            className={cn(
              styles.primaryButton,
              !canSubmit && "cursor-not-allowed opacity-60",
            )}
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
            className={styles.secondaryButton}
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

/** Theme-aware Tailwind class recipes for the event create/edit form. */
const styles: Record<string, string> = {
  root: "mx-auto max-w-[800px] p-6",
  header: "mb-5",
  title: "m-0 text-2xl font-semibold tracking-tight text-fg",
  error:
    "mb-3 flex items-center justify-between gap-2 rounded-lg bg-danger-bg px-3 py-2.5 text-sm text-danger-fg",
  errorDismiss:
    "cursor-pointer rounded-md border-0 bg-transparent px-1.5 text-lg leading-none text-danger-fg hover:bg-danger-bg",
  muted: "text-sm italic text-fg-muted",
  form: "flex flex-col gap-3 rounded-xl border border-border bg-surface p-6 shadow-sm",
  row: "grid grid-cols-[110px_1fr] items-center gap-3",
  bodyRow: "grid grid-cols-[110px_1fr] items-start gap-3",
  label: "text-sm font-medium text-fg-muted",
  input:
    "rounded-lg border border-border bg-surface px-3 py-2 text-sm text-fg outline-none transition-colors placeholder:text-fg-subtle focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  select:
    "rounded-lg border border-border bg-surface px-3 py-2 text-sm text-fg outline-none transition-colors focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  textarea:
    "min-h-28 resize-y rounded-lg border border-border bg-surface p-3 font-[inherit] text-sm text-fg outline-none transition-colors focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  inlineCheckbox: "flex items-center gap-2 text-sm text-fg",
  buttonRow: "mt-4 flex items-center gap-3",
  primaryButton:
    "cursor-pointer rounded-lg border-0 bg-primary px-5 py-2.5 text-sm font-semibold text-primary-fg shadow-sm transition-colors hover:bg-primary-hover",
  secondaryButton:
    "cursor-pointer rounded-lg border border-border bg-surface px-5 py-2.5 text-sm font-semibold text-fg transition-colors hover:bg-surface-hover",
};

interface FreeBusyResp {
  busy?: { start: string; end: string }[];
}

/**
 * FreeBusyCheck (Phase 8) renders a "Check availability" button.
 * The current account ID is read from the existing JMAP session;
 * we issue a GET against the BFF's free/busy publisher and render
 * the resulting busy intervals inline.
 */
function FreeBusyCheck({
  calendarId,
  startLocal,
  endLocal,
}: {
  calendarId: string;
  startLocal: string;
  endLocal: string;
}) {
  const [busy, setBusy] = useState<{ start: string; end: string }[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onClick = async () => {
    setError(null);
    setBusy(null);
    if (!calendarId || !startLocal || !endLocal) {
      setError("Calendar, start and end are required");
      return;
    }
    setLoading(true);
    try {
      const startIso = encodeURIComponent(new Date(startLocal).toISOString());
      const endIso = encodeURIComponent(new Date(endLocal).toISOString());
      const accountId = "default"; // resolved server-side from auth context
      const res = await fetch(
        `/api/v1/calendars/${encodeURIComponent(accountId)}/${encodeURIComponent(
          calendarId,
        )}/freebusy?start=${startIso}&end=${endIso}`,
        { credentials: "include", headers: { Accept: "application/json" } },
      );
      if (!res.ok) {
        throw new Error(`HTTP ${res.status}`);
      }
      const body = (await res.json()) as FreeBusyResp;
      setBusy(body.busy ?? []);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className={styles.row}>
      <label className={styles.label}>Availability</label>
      <div>
        <button
          type="button"
          onClick={onClick}
          disabled={loading}
          className={styles.secondaryButton}
        >
          {loading ? "Checking…" : "Check availability"}
        </button>
        {error && <span className="ml-2 text-danger-fg">{error}</span>}
        {busy && busy.length === 0 && (
          <span className="ml-2 text-success-fg">
            All clear in this window.
          </span>
        )}
        {busy && busy.length > 0 && (
          <ul className="mt-2">
            {busy.map((b, i) => (
              <li key={i} className="text-warning-fg">
                Busy: {new Date(b.start).toLocaleString()} – {new Date(b.end).toLocaleString()}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
