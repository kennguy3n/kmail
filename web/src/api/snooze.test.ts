/**
 * Unit tests for the Email Snooze client.
 *
 * Covers the create contract (POST must return 201), list-envelope
 * unwrapping, the eager-wake DELETE, and the pure preset resolvers
 * that the Inbox snooze menu offers (relative wake times derived
 * from a fixed `now`).
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  defaultSnoozePresets,
  listSnoozes,
  snoozeEmail,
  wakeSnooze,
} from "./snooze";

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

const snapshot = {
  id: "sn1",
  status: "snoozed" as const,
  email_id: "e1",
  snoozed_mailbox_id: "mb-snoozed",
  snooze_until: "2026-02-01T08:00:00Z",
  mark_unread_on_wake: true,
  attempts: 0,
  created_at: "2026-01-01T00:00:00Z",
};

const request = {
  email_id: "e1",
  original_mailbox_ids: { "mb-inbox": true },
  snoozed_mailbox_id: "mb-snoozed",
  snooze_until: "2026-02-01T08:00:00Z",
};

describe("snoozeEmail", () => {
  it("POSTs the request and returns the snapshot on 201", async () => {
    const fetchMock = mockFetch(jsonResponse(snapshot, 201));
    await expect(snoozeEmail(request)).resolves.toMatchObject({ id: "sn1" });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/snooze");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toMatchObject({ email_id: "e1" });
  });

  it("throws when the server does not return 201", async () => {
    mockFetch(new Response("bad", { status: 400 }));
    await expect(snoozeEmail(request)).rejects.toThrow(/snoozeEmail: 400/);
  });
});

describe("listSnoozes", () => {
  it("unwraps the snoozes envelope", async () => {
    mockFetch(jsonResponse({ snoozes: [snapshot] }));
    await expect(listSnoozes()).resolves.toHaveLength(1);
  });
});

describe("wakeSnooze", () => {
  it("DELETEs and resolves cancelled:true on 200", async () => {
    const fetchMock = mockFetch(new Response(null, { status: 200 }));
    await expect(wakeSnooze("sn1")).resolves.toEqual({ cancelled: true });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(init.method).toBe("DELETE");
  });

  it("throws on an unexpected status", async () => {
    mockFetch(new Response("boom", { status: 500 }));
    await expect(wakeSnooze("sn1")).rejects.toThrow(/wakeSnooze: 500/);
  });
});

describe("defaultSnoozePresets", () => {
  it("offers the four conventional quick-picks", () => {
    const presets = defaultSnoozePresets();
    expect(presets.map((p) => p.label)).toEqual([
      "Later today (3 hours)",
      "Tomorrow morning (8 AM)",
      "This weekend (Sat 8 AM)",
      "Next week (Mon 8 AM)",
    ]);
  });

  it("resolves 'Later today' to exactly 3 hours from now", () => {
    const now = new Date("2026-01-05T10:00:00"); // a Monday, local time
    const [laterToday] = defaultSnoozePresets();
    const when = laterToday.resolve(now);
    expect(when.getTime() - now.getTime()).toBe(3 * 60 * 60 * 1000);
  });

  it("resolves 'Tomorrow morning' to 8 AM the next day", () => {
    const now = new Date("2026-01-05T22:00:00");
    const tomorrow = defaultSnoozePresets()[1].resolve(now);
    expect(tomorrow.getDate()).toBe(6);
    expect(tomorrow.getHours()).toBe(8);
    expect(tomorrow.getMinutes()).toBe(0);
  });

  it("resolves weekday presets to the *next* occurrence (never today)", () => {
    // 2026-01-05 is a Monday. "Next week (Mon 8 AM)" must skip to the
    // following Monday rather than returning today.
    const monday = new Date("2026-01-05T08:00:00");
    const nextMonday = defaultSnoozePresets()[3].resolve(monday);
    expect(nextMonday.getDate()).toBe(12);
    expect(nextMonday.getDay()).toBe(1);
    expect(nextMonday.getHours()).toBe(8);
  });
});
