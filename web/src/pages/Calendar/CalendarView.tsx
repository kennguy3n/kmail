import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import type {
  Calendar,
  CalendarEvent,
  EventParticipantResponse,
} from "../../types";

/**
 * CalendarView is the personal / team calendar view.
 *
 * Fetches the authenticated user's calendars and the events that
 * fall in the window defined by `viewMode` + `anchor`, renders
 * them in a time grid (week / day) or a 6-row month grid, and
 * lets the user toggle calendar visibility, inspect an event's
 * details, RSVP, and jump into the event-create form for an
 * empty time slot. The `/calendar/:eventId` route reuses this
 * component (via the `eventId` URL param) and opens the matching
 * event's detail pane on mount so deep links work.
 *
 * All server traffic goes through `jmapClient` which speaks the
 * draft JMAP calendars capability
 * (`urn:ietf:params:jmap:calendars`); when Stalwart v0.16.0 cannot
 * yet answer those methods the Go BFF surfaces the JMAP shape on
 * top of its CalDAV store — this component only talks JMAP.
 */
type ViewMode = "day" | "week" | "month";

const DAY_MS = 24 * 60 * 60 * 1000;

export default function CalendarView() {
  const navigate = useNavigate();
  const { eventId: routeEventId } = useParams<{ eventId?: string }>();

  const [calendars, setCalendars] = useState<Calendar[] | null>(null);
  const [events, setEvents] = useState<CalendarEvent[] | null>(null);
  const [viewMode, setViewMode] = useState<ViewMode>("week");
  const [anchor, setAnchor] = useState<Date>(startOfDay(new Date()));
  const [visibility, setVisibility] = useState<Record<string, boolean>>({});
  const [selectedEvent, setSelectedEvent] = useState<CalendarEvent | null>(
    null,
  );
  const [error, setError] = useState<string | null>(null);
  const [isLoadingCalendars, setLoadingCalendars] = useState(true);
  const [isLoadingEvents, setLoadingEvents] = useState(false);
  // Nonce bumped after a destructive or RSVP write so the event
  // list refetches with the latest server state.
  const [reloadNonce, setReloadNonce] = useState(0);

  const range = useMemo(
    () => computeRange(viewMode, anchor),
    [viewMode, anchor],
  );

  useEffect(() => {
    let cancelled = false;
    setLoadingCalendars(true);
    jmapClient
      .getCalendars()
      .then((list) => {
        if (cancelled) return;
        setCalendars(list);
        setVisibility((prev) => {
          const next: Record<string, boolean> = {};
          for (const c of list) {
            next[c.id] = prev[c.id] ?? c.isVisible;
          }
          return next;
        });
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
  }, []);

  const visibleCalendarIds = useMemo(
    () =>
      (calendars ?? [])
        .filter((c) => visibility[c.id] ?? c.isVisible)
        .map((c) => c.id),
    [calendars, visibility],
  );

  useEffect(() => {
    if (!calendars) return;
    let cancelled = false;
    setLoadingEvents(true);
    const scope =
      visibleCalendarIds.length === 0
        ? null
        : visibleCalendarIds.length === calendars.length
          ? null
          : visibleCalendarIds;
    jmapClient
      .getEvents(scope, {
        start: range.start.toISOString(),
        end: range.end.toISOString(),
      })
      .then((list) => {
        if (cancelled) return;
        // Belt-and-braces client-side filter in case the BFF ignores
        // an empty calendar filter and returns events from every
        // visible calendar.
        const allowed = new Set(visibleCalendarIds);
        setEvents(
          visibleCalendarIds.length === 0
            ? []
            : list.filter((e) => allowed.has(e.calendarId)),
        );
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoadingEvents(false);
      });
    return () => {
      cancelled = true;
    };
  }, [calendars, visibleCalendarIds, range.start, range.end, reloadNonce]);

  // Deep link: /calendar/:eventId pops the corresponding event's
  // detail panel on mount. Once the events list loads we resolve
  // the id; if it is not in the current window we fetch it
  // explicitly so the panel still opens. `resolvedRouteIdRef`
  // tracks which route id has already been resolved so a later
  // user click on a different event does not re-trigger the
  // resolver and snap the selection back.
  const resolvedRouteIdRef = useRef<string | null>(null);
  useEffect(() => {
    if (!routeEventId) {
      resolvedRouteIdRef.current = null;
      return;
    }
    if (resolvedRouteIdRef.current === routeEventId) return;
    const fromList = (events ?? []).find((e) => e.id === routeEventId);
    if (fromList) {
      resolvedRouteIdRef.current = routeEventId;
      setSelectedEvent(fromList);
      return;
    }
    let cancelled = false;
    jmapClient
      .getEvent(routeEventId)
      .then((e) => {
        if (cancelled) return;
        resolvedRouteIdRef.current = routeEventId;
        setSelectedEvent(e);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      });
    return () => {
      cancelled = true;
    };
  }, [routeEventId, events]);

  const handleToggleCalendar = useCallback((calendarId: string) => {
    setVisibility((prev) => ({
      ...prev,
      [calendarId]: !(prev[calendarId] ?? true),
    }));
  }, []);

  const handleSlotClick = useCallback(
    (slotStart: Date) => {
      const end = new Date(slotStart.getTime() + 60 * 60 * 1000);
      navigate(
        `/calendar/new?start=${encodeURIComponent(slotStart.toISOString())}&end=${encodeURIComponent(end.toISOString())}`,
      );
    },
    [navigate],
  );

  const handleRsvp = useCallback(
    async (event: CalendarEvent, response: EventParticipantResponse) => {
      setError(null);
      try {
        await jmapClient.respondToEvent(event.id, response);
        setReloadNonce((n) => n + 1);
      } catch (err: unknown) {
        setError(errorMessage(err));
      }
    },
    [],
  );

  const handleDeleteEvent = useCallback(
    async (event: CalendarEvent) => {
      setError(null);
      try {
        await jmapClient.deleteEvent(event.id);
        setSelectedEvent(null);
        setReloadNonce((n) => n + 1);
        if (routeEventId === event.id) {
          navigate("/calendar");
        }
      } catch (err: unknown) {
        setError(errorMessage(err));
      }
    },
    [navigate, routeEventId],
  );

  const goPrev = useCallback(() => {
    setAnchor((prev) => shiftAnchor(prev, viewMode, -1));
  }, [viewMode]);
  const goNext = useCallback(() => {
    setAnchor((prev) => shiftAnchor(prev, viewMode, 1));
  }, [viewMode]);
  const goToday = useCallback(() => {
    setAnchor(startOfDay(new Date()));
  }, []);

  return (
    <section style={styles.root}>
      <aside style={styles.sidebar}>
        <div style={styles.sidebarHeader}>
          <h2 style={styles.sidebarTitle}>Calendar</h2>
          <button
            type="button"
            onClick={() => navigate("/calendar/new")}
            style={styles.newEventButton}
          >
            New event
          </button>
        </div>
        {isLoadingCalendars ? (
          <p style={styles.muted}>Loading calendars…</p>
        ) : (calendars ?? []).length === 0 ? (
          <p style={styles.muted}>No calendars.</p>
        ) : (
          <ul style={styles.calendarList}>
            {(calendars ?? []).map((c) => {
              const checked = visibility[c.id] ?? c.isVisible;
              return (
                <li key={c.id} style={styles.calendarItem}>
                  <label style={styles.calendarLabel}>
                    <input
                      type="checkbox"
                      checked={checked}
                      onChange={() => handleToggleCalendar(c.id)}
                    />
                    <span
                      style={{
                        ...styles.colorSwatch,
                        background: c.color || "#6366f1",
                      }}
                    />
                    <span>{c.name}</span>
                    {c.isDefault && (
                      <span style={styles.defaultBadge}>default</span>
                    )}
                  </label>
                </li>
              );
            })}
          </ul>
        )}
      </aside>
      <main style={styles.main}>
        <header style={styles.toolbar}>
          <div style={styles.toolbarLeft}>
            <button type="button" onClick={goPrev} style={styles.navButton}>
              ‹
            </button>
            <button type="button" onClick={goToday} style={styles.todayButton}>
              Today
            </button>
            <button type="button" onClick={goNext} style={styles.navButton}>
              ›
            </button>
            <span style={styles.rangeLabel}>
              {formatRange(viewMode, anchor, range)}
            </span>
          </div>
          <div style={styles.viewToggle} role="tablist" aria-label="View mode">
            {(["day", "week", "month"] as const).map((mode) => (
              <button
                key={mode}
                type="button"
                role="tab"
                aria-selected={viewMode === mode}
                onClick={() => setViewMode(mode)}
                style={{
                  ...styles.viewToggleButton,
                  ...(viewMode === mode ? styles.viewToggleButtonActive : {}),
                }}
              >
                {mode[0].toUpperCase() + mode.slice(1)}
              </button>
            ))}
          </div>
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
        {isLoadingEvents && <p style={styles.muted}>Loading events…</p>}
        {viewMode === "month" ? (
          <MonthGrid
            anchor={anchor}
            events={events ?? []}
            calendars={calendars ?? []}
            onEventClick={(e) => setSelectedEvent(e)}
            onDayClick={(d) => handleSlotClick(d)}
          />
        ) : (
          <TimeGrid
            days={buildDays(range.start, viewMode === "day" ? 1 : 7)}
            events={events ?? []}
            calendars={calendars ?? []}
            onEventClick={(e) => setSelectedEvent(e)}
            onSlotClick={handleSlotClick}
          />
        )}
      </main>
      {selectedEvent && (
        <EventDetailsPanel
          event={selectedEvent}
          calendar={(calendars ?? []).find(
            (c) => c.id === selectedEvent.calendarId,
          )}
          onClose={() => {
            setSelectedEvent(null);
            if (routeEventId) navigate("/calendar");
          }}
          onEdit={() => navigate(`/calendar/${selectedEvent.id}/edit`)}
          onDelete={() => void handleDeleteEvent(selectedEvent)}
          onRsvp={(resp) => void handleRsvp(selectedEvent, resp)}
        />
      )}
    </section>
  );
}

interface TimeGridProps {
  days: Date[];
  events: CalendarEvent[];
  calendars: Calendar[];
  onEventClick: (event: CalendarEvent) => void;
  onSlotClick: (slotStart: Date) => void;
}

function TimeGrid({
  days,
  events,
  calendars,
  onEventClick,
  onSlotClick,
}: TimeGridProps) {
  const hours = useMemo(() => Array.from({ length: 24 }, (_, i) => i), []);
  const colorByCalendar = useMemo(() => {
    const m = new Map<string, string>();
    for (const c of calendars) m.set(c.id, c.color || "#6366f1");
    return m;
  }, [calendars]);

  const eventsByDay = useMemo(() => {
    const m = new Map<string, CalendarEvent[]>();
    for (const d of days) m.set(dayKey(d), []);
    for (const ev of events) {
      const start = new Date(ev.start);
      const key = dayKey(start);
      if (m.has(key)) m.get(key)!.push(ev);
    }
    return m;
  }, [days, events]);

  return (
    <div
      style={{
        ...styles.timeGrid,
        gridTemplateColumns: `60px repeat(${days.length}, 1fr)`,
      }}
    >
      <div style={styles.timeGutterHeader} />
      {days.map((d) => (
        <div key={dayKey(d)} style={styles.dayHeader}>
          <div style={styles.dayHeaderDow}>
            {d.toLocaleDateString(undefined, { weekday: "short" })}
          </div>
          <div style={styles.dayHeaderDom}>{d.getDate()}</div>
        </div>
      ))}
      {hours.map((h) => (
        <Row
          key={h}
          hour={h}
          days={days}
          eventsByDay={eventsByDay}
          colorByCalendar={colorByCalendar}
          onEventClick={onEventClick}
          onSlotClick={onSlotClick}
        />
      ))}
    </div>
  );
}

interface RowProps {
  hour: number;
  days: Date[];
  eventsByDay: Map<string, CalendarEvent[]>;
  colorByCalendar: Map<string, string>;
  onEventClick: (event: CalendarEvent) => void;
  onSlotClick: (slotStart: Date) => void;
}

function Row({
  hour,
  days,
  eventsByDay,
  colorByCalendar,
  onEventClick,
  onSlotClick,
}: RowProps) {
  return (
    <>
      <div style={styles.timeGutter}>{formatHour(hour)}</div>
      {days.map((d) => {
        const slotStart = new Date(d);
        slotStart.setHours(hour, 0, 0, 0);
        const slotEnd = new Date(slotStart.getTime() + 60 * 60 * 1000);
        const hits = (eventsByDay.get(dayKey(d)) ?? []).filter((ev) => {
          const s = new Date(ev.start).getTime();
          return s >= slotStart.getTime() && s < slotEnd.getTime();
        });
        return (
          <button
            key={`${dayKey(d)}-${hour}`}
            type="button"
            onClick={() => onSlotClick(slotStart)}
            style={styles.timeSlot}
            aria-label={`Create event at ${slotStart.toLocaleString()}`}
          >
            {hits.map((ev) => (
              <EventChip
                key={ev.id}
                event={ev}
                color={colorByCalendar.get(ev.calendarId)}
                onClick={(e) => {
                  e.stopPropagation();
                  onEventClick(ev);
                }}
              />
            ))}
          </button>
        );
      })}
    </>
  );
}

interface EventChipProps {
  event: CalendarEvent;
  color: string | undefined;
  onClick: (e: React.MouseEvent) => void;
}

function EventChip({ event, color, onClick }: EventChipProps) {
  const start = new Date(event.start);
  const end = new Date(event.end);
  return (
    <span
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick(e as unknown as React.MouseEvent);
        }
      }}
      style={{
        ...styles.eventChip,
        background: color ?? "#6366f1",
      }}
    >
      <span style={styles.eventChipTitle}>{event.title}</span>
      <span style={styles.eventChipTime}>
        {formatTime(start)}–{formatTime(end)}
      </span>
    </span>
  );
}

interface MonthGridProps {
  anchor: Date;
  events: CalendarEvent[];
  calendars: Calendar[];
  onEventClick: (event: CalendarEvent) => void;
  onDayClick: (day: Date) => void;
}

function MonthGrid({
  anchor,
  events,
  calendars,
  onEventClick,
  onDayClick,
}: MonthGridProps) {
  const colorByCalendar = useMemo(() => {
    const m = new Map<string, string>();
    for (const c of calendars) m.set(c.id, c.color || "#6366f1");
    return m;
  }, [calendars]);
  const cells = useMemo(() => {
    const first = new Date(anchor.getFullYear(), anchor.getMonth(), 1);
    const start = new Date(first);
    start.setDate(1 - first.getDay());
    return Array.from({ length: 42 }, (_, i) => {
      const d = new Date(start);
      d.setDate(start.getDate() + i);
      return d;
    });
  }, [anchor]);
  const eventsByDay = useMemo(() => {
    const m = new Map<string, CalendarEvent[]>();
    for (const ev of events) {
      const key = dayKey(new Date(ev.start));
      const arr = m.get(key) ?? [];
      arr.push(ev);
      m.set(key, arr);
    }
    return m;
  }, [events]);
  const weekdays = useMemo(() => {
    return Array.from({ length: 7 }, (_, i) => {
      const d = new Date(2024, 5, 2 + i);
      return d.toLocaleDateString(undefined, { weekday: "short" });
    });
  }, []);
  const monthIndex = anchor.getMonth();
  return (
    <div style={styles.monthGrid}>
      {weekdays.map((w) => (
        <div key={w} style={styles.monthHeader}>
          {w}
        </div>
      ))}
      {cells.map((d) => {
        const inMonth = d.getMonth() === monthIndex;
        const hits = eventsByDay.get(dayKey(d)) ?? [];
        return (
          <button
            key={d.toISOString()}
            type="button"
            onClick={() => onDayClick(startOfDay(d))}
            style={{
              ...styles.monthCell,
              ...(inMonth ? {} : styles.monthCellOut),
            }}
          >
            <span style={styles.monthDom}>{d.getDate()}</span>
            <span style={styles.monthEvents}>
              {hits.slice(0, 3).map((ev) => (
                <span
                  key={ev.id}
                  role="button"
                  tabIndex={0}
                  onClick={(e) => {
                    e.stopPropagation();
                    onEventClick(ev);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.stopPropagation();
                      e.preventDefault();
                      onEventClick(ev);
                    }
                  }}
                  style={{
                    ...styles.monthEventChip,
                    background: colorByCalendar.get(ev.calendarId) ?? "#6366f1",
                  }}
                >
                  {ev.title}
                </span>
              ))}
              {hits.length > 3 && (
                <span style={styles.monthMore}>+{hits.length - 3} more</span>
              )}
            </span>
          </button>
        );
      })}
    </div>
  );
}

interface EventDetailsPanelProps {
  event: CalendarEvent;
  calendar: Calendar | undefined;
  onClose: () => void;
  onEdit: () => void;
  onDelete: () => void;
  onRsvp: (response: EventParticipantResponse) => void;
}

function EventDetailsPanel({
  event,
  calendar,
  onClose,
  onEdit,
  onDelete,
  onRsvp,
}: EventDetailsPanelProps) {
  const start = new Date(event.start);
  const end = new Date(event.end);
  return (
    <aside style={styles.detailsPanel} aria-label="Event details">
      <div style={styles.detailsHeader}>
        <h3 style={styles.detailsTitle}>{event.title}</h3>
        <button
          type="button"
          onClick={onClose}
          style={styles.detailsClose}
          aria-label="Close event details"
        >
          ×
        </button>
      </div>
      <p style={styles.detailsRow}>
        <strong>When: </strong>
        {start.toLocaleString()} – {end.toLocaleString()}
      </p>
      {calendar && (
        <p style={styles.detailsRow}>
          <strong>Calendar: </strong>
          <span
            style={{
              ...styles.colorSwatch,
              background: calendar.color || "#6366f1",
            }}
          />
          {calendar.name}
        </p>
      )}
      {event.location && (
        <p style={styles.detailsRow}>
          <strong>Location: </strong>
          {event.location}
        </p>
      )}
      {event.description && (
        <p style={styles.detailsRow}>
          <strong>Description: </strong>
          {event.description}
        </p>
      )}
      {event.participants && event.participants.length > 0 && (
        <div style={styles.detailsRow}>
          <strong>Participants:</strong>
          <ul style={styles.participantList}>
            {event.participants.map((p) => (
              <li key={p.email}>
                {p.name ? `${p.name} <${p.email}>` : p.email}
                {p.rsvp && <span style={styles.rsvpBadge}> — {p.rsvp}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      <div style={styles.detailsActions}>
        <button
          type="button"
          onClick={() => onRsvp("accepted")}
          style={styles.rsvpAccept}
        >
          Accept
        </button>
        <button
          type="button"
          onClick={() => onRsvp("tentative")}
          style={styles.rsvpTentative}
        >
          Tentative
        </button>
        <button
          type="button"
          onClick={() => onRsvp("declined")}
          style={styles.rsvpDecline}
        >
          Decline
        </button>
      </div>
      <div style={styles.detailsActions}>
        <button type="button" onClick={onEdit} style={styles.editButton}>
          Edit
        </button>
        <button type="button" onClick={onDelete} style={styles.deleteButton}>
          Delete
        </button>
      </div>
    </aside>
  );
}

function computeRange(
  mode: ViewMode,
  anchor: Date,
): { start: Date; end: Date } {
  if (mode === "day") {
    const start = startOfDay(anchor);
    const end = new Date(start.getTime() + DAY_MS);
    return { start, end };
  }
  if (mode === "week") {
    const start = startOfDay(anchor);
    start.setDate(start.getDate() - start.getDay());
    const end = new Date(start.getTime() + 7 * DAY_MS);
    return { start, end };
  }
  const start = new Date(anchor.getFullYear(), anchor.getMonth(), 1);
  const gridStart = new Date(start);
  gridStart.setDate(1 - start.getDay());
  const end = new Date(gridStart.getTime() + 42 * DAY_MS);
  return { start: gridStart, end };
}

function shiftAnchor(anchor: Date, mode: ViewMode, direction: -1 | 1): Date {
  const next = new Date(anchor);
  if (mode === "day") {
    next.setDate(next.getDate() + direction);
  } else if (mode === "week") {
    next.setDate(next.getDate() + direction * 7);
  } else {
    next.setMonth(next.getMonth() + direction);
  }
  return next;
}

function buildDays(start: Date, count: number): Date[] {
  return Array.from({ length: count }, (_, i) => {
    const d = new Date(start);
    d.setDate(start.getDate() + i);
    return startOfDay(d);
  });
}

function startOfDay(d: Date): Date {
  const out = new Date(d);
  out.setHours(0, 0, 0, 0);
  return out;
}

function dayKey(d: Date): string {
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

function formatHour(h: number): string {
  const d = new Date();
  d.setHours(h, 0, 0, 0);
  return d.toLocaleTimeString(undefined, { hour: "numeric" });
}

function formatTime(d: Date): string {
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

function formatRange(
  mode: ViewMode,
  anchor: Date,
  range: { start: Date; end: Date },
): string {
  if (mode === "day") {
    return anchor.toLocaleDateString(undefined, {
      weekday: "long",
      month: "long",
      day: "numeric",
      year: "numeric",
    });
  }
  if (mode === "week") {
    const last = new Date(range.end.getTime() - DAY_MS);
    return `${range.start.toLocaleDateString(undefined, { month: "short", day: "numeric" })} – ${last.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })}`;
  }
  return anchor.toLocaleDateString(undefined, {
    month: "long",
    year: "numeric",
  });
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Unknown error";
}

const styles: Record<string, React.CSSProperties> = {
  root: {
    display: "grid",
    gridTemplateColumns: "220px 1fr",
    minHeight: "calc(100vh - 4rem)",
    gap: "1rem",
    position: "relative",
  },
  sidebar: {
    borderRight: "1px solid #e5e7eb",
    padding: "1rem",
    background: "#f9fafb",
  },
  sidebarHeader: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.75rem",
  },
  sidebarTitle: {
    margin: 0,
    fontSize: "1.1rem",
  },
  newEventButton: {
    padding: "0.25rem 0.5rem",
    fontSize: "0.85rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  calendarList: {
    listStyle: "none",
    margin: 0,
    padding: 0,
    display: "flex",
    flexDirection: "column",
    gap: "0.25rem",
  },
  calendarItem: {
    padding: "0.25rem 0",
  },
  calendarLabel: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.9rem",
    cursor: "pointer",
  },
  colorSwatch: {
    display: "inline-block",
    width: "0.75rem",
    height: "0.75rem",
    borderRadius: "0.15rem",
    verticalAlign: "middle",
    marginRight: "0.25rem",
  },
  defaultBadge: {
    fontSize: "0.7rem",
    color: "#6b7280",
    marginLeft: "auto",
  },
  main: {
    padding: "1rem",
    minWidth: 0,
  },
  toolbar: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.75rem",
    gap: "0.75rem",
    flexWrap: "wrap",
  },
  toolbarLeft: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
  },
  navButton: {
    padding: "0.25rem 0.5rem",
    fontSize: "1rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    lineHeight: 1,
  },
  todayButton: {
    padding: "0.3rem 0.75rem",
    fontSize: "0.85rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  rangeLabel: {
    marginLeft: "0.5rem",
    fontSize: "1rem",
    fontWeight: 600,
    color: "#111827",
  },
  viewToggle: {
    display: "inline-flex",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    overflow: "hidden",
  },
  viewToggleButton: {
    padding: "0.3rem 0.75rem",
    fontSize: "0.85rem",
    background: "#fff",
    border: "none",
    cursor: "pointer",
    borderRight: "1px solid #d1d5db",
  },
  viewToggleButtonActive: {
    background: "#dbeafe",
    fontWeight: 600,
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
  timeGrid: {
    display: "grid",
    border: "1px solid #e5e7eb",
    borderRadius: "0.25rem",
    overflow: "hidden",
    background: "#fff",
  },
  timeGutterHeader: {
    borderBottom: "1px solid #e5e7eb",
    background: "#f9fafb",
  },
  dayHeader: {
    padding: "0.4rem",
    textAlign: "center",
    borderBottom: "1px solid #e5e7eb",
    borderLeft: "1px solid #e5e7eb",
    background: "#f9fafb",
  },
  dayHeaderDow: {
    fontSize: "0.75rem",
    color: "#6b7280",
    textTransform: "uppercase",
  },
  dayHeaderDom: {
    fontSize: "1rem",
    fontWeight: 600,
  },
  timeGutter: {
    padding: "0.25rem 0.4rem",
    fontSize: "0.7rem",
    color: "#6b7280",
    textAlign: "right",
    borderBottom: "1px solid #f3f4f6",
    borderRight: "1px solid #e5e7eb",
  },
  timeSlot: {
    minHeight: "2.5rem",
    borderLeft: "1px solid #e5e7eb",
    borderBottom: "1px solid #f3f4f6",
    padding: "0.2rem",
    background: "#fff",
    textAlign: "left",
    cursor: "pointer",
    font: "inherit",
    position: "relative",
    display: "flex",
    flexDirection: "column",
    gap: "0.15rem",
  },
  eventChip: {
    display: "flex",
    flexDirection: "column",
    padding: "0.2rem 0.35rem",
    borderRadius: "0.2rem",
    color: "#fff",
    fontSize: "0.75rem",
    cursor: "pointer",
    lineHeight: 1.2,
  },
  eventChipTitle: {
    fontWeight: 600,
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
  },
  eventChipTime: {
    fontSize: "0.65rem",
    opacity: 0.9,
  },
  monthGrid: {
    display: "grid",
    gridTemplateColumns: "repeat(7, 1fr)",
    gridAutoRows: "minmax(5rem, 1fr)",
    border: "1px solid #e5e7eb",
    borderRadius: "0.25rem",
    overflow: "hidden",
    background: "#fff",
  },
  monthHeader: {
    padding: "0.4rem",
    fontSize: "0.75rem",
    color: "#6b7280",
    textTransform: "uppercase",
    textAlign: "center",
    background: "#f9fafb",
    borderBottom: "1px solid #e5e7eb",
    borderLeft: "1px solid #e5e7eb",
  },
  monthCell: {
    padding: "0.25rem",
    borderLeft: "1px solid #e5e7eb",
    borderTop: "1px solid #f3f4f6",
    background: "#fff",
    textAlign: "left",
    cursor: "pointer",
    font: "inherit",
    display: "flex",
    flexDirection: "column",
    gap: "0.15rem",
  },
  monthCellOut: {
    background: "#f9fafb",
    color: "#9ca3af",
  },
  monthDom: {
    fontSize: "0.85rem",
    fontWeight: 600,
  },
  monthEvents: {
    display: "flex",
    flexDirection: "column",
    gap: "0.15rem",
  },
  monthEventChip: {
    fontSize: "0.7rem",
    color: "#fff",
    padding: "0.1rem 0.3rem",
    borderRadius: "0.2rem",
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
    cursor: "pointer",
  },
  monthMore: {
    fontSize: "0.7rem",
    color: "#6b7280",
  },
  detailsPanel: {
    position: "fixed",
    right: "1rem",
    top: "5rem",
    width: "320px",
    maxHeight: "calc(100vh - 6rem)",
    overflowY: "auto",
    background: "#fff",
    border: "1px solid #e5e7eb",
    borderRadius: "0.5rem",
    boxShadow: "0 10px 25px rgba(0,0,0,0.1)",
    padding: "1rem",
    zIndex: 50,
  },
  detailsHeader: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.5rem",
  },
  detailsTitle: {
    margin: 0,
    fontSize: "1.1rem",
  },
  detailsClose: {
    background: "transparent",
    border: "none",
    fontSize: "1.25rem",
    cursor: "pointer",
    lineHeight: 1,
  },
  detailsRow: {
    fontSize: "0.85rem",
    margin: "0.25rem 0",
  },
  participantList: {
    listStyle: "none",
    margin: "0.25rem 0 0",
    padding: 0,
    fontSize: "0.85rem",
  },
  rsvpBadge: {
    fontSize: "0.75rem",
    color: "#6b7280",
    fontStyle: "italic",
  },
  detailsActions: {
    display: "flex",
    gap: "0.4rem",
    marginTop: "0.75rem",
  },
  rsvpAccept: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#16a34a",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  rsvpTentative: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#f59e0b",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  rsvpDecline: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#dc2626",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  editButton: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  deleteButton: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.8rem",
    background: "#fff",
    color: "#991b1b",
    border: "1px solid #fecaca",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
};
