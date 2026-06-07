/**
 * Unit tests for the Scheduled Send client.
 *
 * Covers the list-envelope unwrapping (`{ scheduled_sends: [...] }`),
 * the single-row read, and the cancel tri-state (200 → cancelled,
 * 410 → already dispatched, else → throw).
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  cancelScheduledSend,
  getScheduledSend,
  listScheduledSends,
} from "./scheduledSend";

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

const row = {
  id: "ss1",
  status: "pending" as const,
  email_id: "e1",
  identity_id: "id1",
  send_at: "2026-02-01T09:00:00Z",
  attempts: 0,
  created_at: "2026-01-01T00:00:00Z",
};

describe("listScheduledSends", () => {
  it("unwraps the scheduled_sends envelope", async () => {
    const fetchMock = mockFetch(jsonResponse({ scheduled_sends: [row] }));
    const rows = await listScheduledSends();
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe("ss1");
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/scheduled-sends");
  });

  it("tolerates a missing envelope array", async () => {
    mockFetch(jsonResponse({}));
    await expect(listScheduledSends()).resolves.toEqual([]);
  });

  it("throws on a non-ok response", async () => {
    mockFetch(new Response("boom", { status: 500 }));
    await expect(listScheduledSends()).rejects.toThrow(/listScheduledSends: 500/);
  });
});

describe("getScheduledSend", () => {
  it("URL-encodes the id and returns the snapshot", async () => {
    const fetchMock = mockFetch(jsonResponse(row));
    await expect(getScheduledSend("ss 1")).resolves.toMatchObject({ id: "ss1" });
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/scheduled-sends/ss%201");
  });
});

describe("cancelScheduledSend", () => {
  it("resolves cancelled:true on 200", async () => {
    mockFetch(new Response(null, { status: 200 }));
    await expect(cancelScheduledSend("ss1")).resolves.toEqual({
      cancelled: true,
    });
  });

  it("resolves cancelled:false on 410 Gone", async () => {
    mockFetch(new Response("gone", { status: 410 }));
    await expect(cancelScheduledSend("ss1")).resolves.toEqual({
      cancelled: false,
    });
  });

  it("throws on an unexpected status", async () => {
    mockFetch(new Response("boom", { status: 500 }));
    await expect(cancelScheduledSend("ss1")).rejects.toThrow(
      /cancelScheduledSend: 500/,
    );
  });
});
