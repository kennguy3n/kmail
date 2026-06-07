/**
 * Unit tests for the calendar-sharing client.
 *
 * These routes flow through the admin `requestJSON` helper, so the
 * tests assert the URL + method + admin auth header wiring, the
 * envelope unwrapping for the list endpoints (`{ shares }` /
 * `{ resources }`), and the JSON request bodies.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createCalendar,
  listResourceCalendars,
  listSharedCalendars,
  shareCalendar,
} from "./calendarSharing";
import { DEV_BEARER_TOKEN } from "./jmap";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function mockFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn();
  for (const r of responses) fetchMock.mockResolvedValueOnce(r);
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("createCalendar", () => {
  it("POSTs the calendar payload with admin auth", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "cal-1", name: "Team" }));
    const created = await createCalendar({
      name: "Team",
      calendar_type: "shared",
    });
    expect(created).toEqual({ id: "cal-1", name: "Team" });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/calendars");
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${DEV_BEARER_TOKEN}`,
    );
    expect(JSON.parse(init.body as string)).toMatchObject({
      name: "Team",
      calendar_type: "shared",
    });
  });
});

describe("shareCalendar", () => {
  it("POSTs the share grant to the calendar-scoped endpoint", async () => {
    const fetchMock = mockFetch(
      jsonResponse({
        id: "share-1",
        calendar_id: "cal 1",
        target_account_id: "acct-2",
        permission: "readwrite",
      }),
    );
    const share = await shareCalendar("cal 1", "acct-2", "readwrite");
    expect(share.permission).toBe("readwrite");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/calendars/cal%201/share");
    expect(JSON.parse(init.body as string)).toEqual({
      target_account_id: "acct-2",
      permission: "readwrite",
    });
  });
});

describe("listSharedCalendars", () => {
  it("unwraps the shares envelope", async () => {
    mockFetch(jsonResponse({ shares: [{ id: "s1" }] }));
    await expect(listSharedCalendars()).resolves.toHaveLength(1);
  });

  it("tolerates a missing envelope", async () => {
    mockFetch(jsonResponse({}));
    await expect(listSharedCalendars()).resolves.toEqual([]);
  });
});

describe("listResourceCalendars", () => {
  it("unwraps the resources envelope", async () => {
    mockFetch(jsonResponse({ resources: [{ id: "room-1" }] }));
    await expect(listResourceCalendars()).resolves.toHaveLength(1);
  });
});
