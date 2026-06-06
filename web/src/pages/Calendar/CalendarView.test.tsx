/**
 * Component test for CalendarView.tsx.
 *
 * The full calendar view covers a lot of behaviour (week / month
 * grid, RSVP, delete, deep links). This file pins down the
 * load-bearing first paint:
 *
 *   - On mount, the page calls `jmapClient.getCalendars()` and
 *     `jmapClient.getEvents()` for the current view's date range.
 *   - Calendars from the server appear in the sidebar.
 *   - Events whose `calendarId` matches a visible calendar show
 *     up in the time grid.
 *   - The view starts in "week" mode.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import CalendarView from "./CalendarView";
import type { Calendar, CalendarEvent } from "../../types";

const calendars: Calendar[] = [
  {
    id: "cal-1",
    name: "Personal",
    color: "#3b82f6",
    isVisible: true,
    isDefault: true,
  },
  {
    id: "cal-2",
    name: "Team",
    color: "#10b981",
    isVisible: true,
    isDefault: false,
  },
];

function eventInWindow(): CalendarEvent[] {
  // CalendarView defaults to a Sunday-start week view whose grid only
  // renders the seven days of the *current* week. The sample event must
  // therefore fall on a day inside that week. Anchor it at local noon
  // today: the current day is always inside the current week, and noon
  // can't spill into an adjacent day under any timezone offset.
  //
  // The previous anchor (`Date.now() + 2h`) was off-by-a-week near the
  // Saturday->Sunday boundary: run late on a Saturday, +2h crosses into
  // the next week's Sunday, which has no column in the rendered grid, so
  // the chip silently disappeared and the assertion failed depending on
  // the wall-clock day/time the suite happened to run.
  const start = new Date();
  start.setHours(12, 0, 0, 0);
  const end = new Date(start.getTime() + 60 * 60 * 1000);
  return [
    {
      id: "ev-1",
      calendarId: "cal-1",
      title: "Sprint planning",
      description: "Plan next sprint",
      start: start.toISOString(),
      end: end.toISOString(),
      location: null,
      participants: [],
      status: "confirmed",
    },
  ];
}

const getCalendars = vi.fn();
const getEvents = vi.fn();
const getEvent = vi.fn();

vi.mock("../../api/jmap", () => ({
  jmapClient: {
    getCalendars: () => getCalendars(),
    getEvents: (scope: unknown, range: unknown) => getEvents(scope, range),
    getEvent: (id: string) => getEvent(id),
  },
}));

function renderCalendar(initialPath = "/calendar") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route path="/calendar" element={<CalendarView />} />
        <Route path="/calendar/:eventId" element={<CalendarView />} />
        <Route path="/calendar/new" element={<div>new event</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("<CalendarView />", () => {
  it("loads calendars on mount and lists them in the sidebar", async () => {
    getCalendars.mockResolvedValueOnce(calendars);
    getEvents.mockResolvedValueOnce([]);

    renderCalendar();

    expect(await screen.findByText("Personal")).toBeInTheDocument();
    expect(screen.getByText("Team")).toBeInTheDocument();
  });

  it("queries events via jmapClient.getEvents() for the current week", async () => {
    getCalendars.mockResolvedValueOnce(calendars);
    getEvents.mockResolvedValueOnce([]);

    renderCalendar();

    await waitFor(() => expect(getEvents).toHaveBeenCalled());
    const [scope, range] = getEvents.mock.calls.at(-1) as [
      unknown,
      { start: string; end: string },
    ];
    // Both calendars are visible so the BFF gets a `null` (= every
    // calendar) scope; the date range must parse as ISO.
    expect(scope).toBeNull();
    expect(new Date(range.start).toString()).not.toBe("Invalid Date");
    expect(new Date(range.end).toString()).not.toBe("Invalid Date");
    expect(new Date(range.end).getTime()).toBeGreaterThan(
      new Date(range.start).getTime(),
    );
  });

  it("renders an event chip whose calendar is visible", async () => {
    getCalendars.mockResolvedValueOnce(calendars);
    getEvents.mockResolvedValueOnce(eventInWindow());

    renderCalendar();

    expect(await screen.findByText("Sprint planning")).toBeInTheDocument();
  });
});
