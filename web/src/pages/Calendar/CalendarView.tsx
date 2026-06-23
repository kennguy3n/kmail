import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { CalendarDays, CalendarPlus } from "lucide-react";

import { cn } from "../../lib/cn";
import { Button } from "../../components/ui/Button";
import { EmptyState } from "../../components/ui/EmptyState";

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
    <section className={styles.root}>
      <aside className={styles.sidebar}>
        <div className={styles.sidebarHeader}>
          <h2 className={styles.sidebarTitle}>Calendar</h2>
          <button
            type="button"
            onClick={() => navigate("/calendar/new")}
            className={styles.newEventButton}
          >
            New event
          </button>
        </div>
        {isLoadingCalendars ? (
          <EmptyState
            icon={<CalendarDays />}
            title="Loading calendars…"
            description="Please wait while we fetch your schedule."
          />
        ) : (calendars ?? []).length === 0 ? (
          <EmptyState
            icon={<CalendarDays />}
            title="No calendars yet"
            description="Create your first calendar to start scheduling events."
            action={
              <Button
                iconLeft={<CalendarPlus />}
                onClick={() => navigate("/calendar/new")}
              >
                New event
              </Button>
            }
          />
        ) : (
          <ul className={styles.calendarList}>
            {(calendars ?? []).map((c) => {
              const checked = visibility[c.id] ?? c.isVisible;
              return (
                <li key={c.id} className={styles.calendarItem}>
                  <label className={styles.calendarLabel}>
                    <input
                      type="checkbox"
                      checked={checked}
                      onChange={() => handleToggleCalendar(c.id)}
                    />
                    <span
                      className={styles.colorSwatch}
                      style={{ background: c.color || "#6366f1" }}
                    />
                    <span>{c.name}</span>
                    {c.isDefault && (
                      <span className={styles.defaultBadge}>default</span>
                    )}
                  </label>
                </li>
              );
            })}
          </ul>
        )}
      </aside>
      <main className={styles.main}>
        <header className={styles.toolbar}>
          <div className={styles.toolbarLeft}>
            <button type="button" onClick={goPrev} className={styles.navButton}>
              ‹
            </button>
            <button type="button" onClick={goToday} className={styles.todayButton}>
              Today
            </button>
            <button type="button" onClick={goNext} className={styles.navButton}>
              ›
            </button>
            <span className={styles.rangeLabel}>
              {formatRange(viewMode, anchor, range)}
            </span>
          </div>
          <div className={styles.viewToggle} role="tablist" aria-label="View mode">
            {(["day", "week", "month"] as const).map((mode) => (
              <button
                key={mode}
                type="button"
                role="tab"
                aria-selected={viewMode === mode}
                onClick={() => setViewMode(mode)}
                className={cn(
                  styles.viewToggleButton,
                  viewMode === mode && styles.viewToggleButtonActive,
                )}
              >
                {mode[0].toUpperCase() + mode.slice(1)}
              </button>
            ))}
          </div>
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
        {isLoadingEvents && <p className={styles.muted}>Loading events…</p>}
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
      className={styles.timeGrid}
      style={{ gridTemplateColumns: `60px repeat(${days.length}, 1fr)` }}
    >
      <div className={styles.timeGutterHeader} />
      {days.map((d) => (
        <div key={dayKey(d)} className={styles.dayHeader}>
          <div className={styles.dayHeaderDow}>
            {d.toLocaleDateString(undefined, { weekday: "short" })}
          </div>
          <div className={styles.dayHeaderDom}>{d.getDate()}</div>
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
      <div className={styles.timeGutter}>{formatHour(hour)}</div>
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
            className={styles.timeSlot}
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
      className={styles.eventChip}
      style={{ background: color ?? "#6366f1" }}
    >
      <span className={styles.eventChipTitle}>{event.title}</span>
      <span className={styles.eventChipTime}>
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
    <div className={styles.monthGrid}>
      {weekdays.map((w) => (
        <div key={w} className={styles.monthHeader}>
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
            className={cn(styles.monthCell, !inMonth && styles.monthCellOut)}
          >
            <span className={styles.monthDom}>{d.getDate()}</span>
            <span className={styles.monthEvents}>
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
                  className={styles.monthEventChip}
                  style={{
                    background: colorByCalendar.get(ev.calendarId) ?? "#6366f1",
                  }}
                >
                  {ev.title}
                </span>
              ))}
              {hits.length > 3 && (
                <span className={styles.monthMore}>+{hits.length - 3} more</span>
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
    <aside className={styles.detailsPanel} aria-label="Event details">
      <div className={styles.detailsHeader}>
        <h3 className={styles.detailsTitle}>{event.title}</h3>
        <button
          type="button"
          onClick={onClose}
          className={styles.detailsClose}
          aria-label="Close event details"
        >
          ×
        </button>
      </div>
      <p className={styles.detailsRow}>
        <strong>When: </strong>
        {start.toLocaleString()} – {end.toLocaleString()}
      </p>
      {calendar && (
        <p className={styles.detailsRow}>
          <strong>Calendar: </strong>
          <span
            className={styles.colorSwatch}
            style={{ background: calendar.color || "#6366f1" }}
          />
          {calendar.name}
        </p>
      )}
      {event.location && (
        <p className={styles.detailsRow}>
          <strong>Location: </strong>
          {event.location}
        </p>
      )}
      {event.description && (
        <p className={styles.detailsRow}>
          <strong>Description: </strong>
          {event.description}
        </p>
      )}
      {event.participants && event.participants.length > 0 && (
        <div className={styles.detailsRow}>
          <strong>Participants:</strong>
          <ul className={styles.participantList}>
            {event.participants.map((p) => (
              <li key={p.email}>
                {p.name ? `${p.name} <${p.email}>` : p.email}
                {p.rsvp && <span className={styles.rsvpBadge}> — {p.rsvp}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}
      <div className={styles.detailsActions}>
        <button
          type="button"
          onClick={() => onRsvp("accepted")}
          className={styles.rsvpAccept}
        >
          Accept
        </button>
        <button
          type="button"
          onClick={() => onRsvp("tentative")}
          className={styles.rsvpTentative}
        >
          Tentative
        </button>
        <button
          type="button"
          onClick={() => onRsvp("declined")}
          className={styles.rsvpDecline}
        >
          Decline
        </button>
      </div>
      <div className={styles.detailsActions}>
        <button type="button" onClick={onEdit} className={styles.editButton}>
          Edit
        </button>
        <button type="button" onClick={onDelete} className={styles.deleteButton}>
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

/** Theme-aware Tailwind class recipes for the calendar view. */
const styles: Record<string, string> = {
  root: "relative grid min-h-[calc(100vh-4rem)] grid-cols-[220px_1fr] gap-4",
  sidebar: "border-r border-border bg-surface-muted p-4",
  sidebarHeader: "mb-3 flex items-center justify-between",
  sidebarTitle: "m-0 text-lg font-semibold",
  newEventButton:
    "cursor-pointer rounded-md border-0 bg-primary px-2 py-1 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  calendarList: "m-0 flex list-none flex-col gap-1 p-0",
  calendarItem: "py-1",
  calendarLabel: "flex cursor-pointer items-center gap-1.5 text-sm",
  colorSwatch: "mr-1 inline-block size-3 rounded-sm align-middle",
  defaultBadge: "ml-auto text-xs text-fg-muted",
  main: "min-w-0 p-4",
  toolbar: "mb-3 flex flex-wrap items-center justify-between gap-3",
  toolbarLeft: "flex items-center gap-2",
  navButton:
    "cursor-pointer rounded-md border border-border bg-surface px-2 py-1 leading-none transition-colors hover:bg-surface-hover",
  todayButton:
    "cursor-pointer rounded-md border border-border bg-surface px-3 py-1.5 text-sm transition-colors hover:bg-surface-hover",
  rangeLabel: "ml-2 text-base font-semibold text-fg",
  viewToggle:
    "inline-flex overflow-hidden rounded-md border border-border",
  viewToggleButton:
    "cursor-pointer border-0 border-r border-border bg-surface px-3 py-1.5 text-sm transition-colors last:border-r-0 hover:bg-surface-hover",
  viewToggleButtonActive: "bg-primary-subtle font-semibold text-primary",
  error:
    "mb-3 flex items-center justify-between gap-2 rounded-md bg-danger-bg px-3 py-2 text-danger-fg",
  errorDismiss:
    "cursor-pointer border-0 bg-transparent px-1 text-lg leading-none text-danger-fg",
  muted: "italic text-fg-muted",
  timeGrid:
    "grid overflow-hidden rounded-md border border-border bg-surface",
  timeGutterHeader: "border-b border-border bg-surface-muted",
  dayHeader:
    "border-b border-l border-border bg-surface-muted p-1.5 text-center",
  dayHeaderDow: "text-xs uppercase text-fg-muted",
  dayHeaderDom: "text-base font-semibold",
  timeGutter:
    "border-b border-r border-border px-1.5 py-1 text-right text-xs text-fg-muted",
  timeSlot:
    "relative flex min-h-10 cursor-pointer flex-col gap-0.5 border-b border-l border-border bg-surface p-1 text-left font-[inherit] transition-colors hover:bg-surface-hover",
  eventChip:
    "flex cursor-pointer flex-col rounded-sm px-1.5 py-1 text-xs leading-tight text-white",
  eventChipTitle: "overflow-hidden text-ellipsis whitespace-nowrap font-semibold",
  eventChipTime: "text-[0.65rem] opacity-90",
  monthGrid:
    "grid auto-rows-[minmax(5rem,1fr)] grid-cols-7 overflow-hidden rounded-md border border-border bg-surface",
  monthHeader:
    "border-b border-l border-border bg-surface-muted p-1.5 text-center text-xs uppercase text-fg-muted",
  monthCell:
    "flex cursor-pointer flex-col gap-0.5 border-l border-t border-border bg-surface p-1 text-left font-[inherit] transition-colors hover:bg-surface-hover",
  monthCellOut: "bg-surface-muted text-fg-subtle",
  monthDom: "text-sm font-semibold",
  monthEvents: "flex flex-col gap-0.5",
  monthEventChip:
    "cursor-pointer overflow-hidden text-ellipsis whitespace-nowrap rounded-sm px-1.5 py-0.5 text-xs text-white",
  monthMore: "text-xs text-fg-muted",
  detailsPanel:
    "fixed right-4 top-20 z-modal max-h-[calc(100vh-6rem)] w-80 overflow-y-auto rounded-lg border border-border bg-elevated p-4 shadow-lg",
  detailsHeader: "mb-2 flex items-center justify-between",
  detailsTitle: "m-0 text-lg font-semibold",
  detailsClose:
    "cursor-pointer border-0 bg-transparent text-xl leading-none text-fg-muted hover:text-fg",
  detailsRow: "my-1 text-sm",
  participantList: "m-0 mt-1 list-none p-0 text-sm",
  rsvpBadge: "text-xs italic text-fg-muted",
  detailsActions: "mt-3 flex gap-1.5",
  rsvpAccept:
    "cursor-pointer rounded-md border-0 bg-success px-2.5 py-1 text-xs text-white transition-opacity hover:opacity-90",
  rsvpTentative:
    "cursor-pointer rounded-md border-0 bg-warning px-2.5 py-1 text-xs text-white transition-opacity hover:opacity-90",
  rsvpDecline:
    "cursor-pointer rounded-md border-0 bg-danger px-2.5 py-1 text-xs text-white transition-opacity hover:opacity-90",
  editButton:
    "cursor-pointer rounded-md border-0 bg-primary px-2.5 py-1 text-xs font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  deleteButton:
    "cursor-pointer rounded-md border border-danger/40 bg-surface px-2.5 py-1 text-xs text-danger-fg transition-colors hover:bg-danger-bg",
};
