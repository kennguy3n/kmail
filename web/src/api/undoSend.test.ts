/**
 * Unit tests for the Undo Send client.
 *
 * The cancel endpoint has tri-state semantics the Compose page
 * depends on: 200 → cancelled, 410 Gone → already dispatched (not
 * cancelled), anything else → throw. The status read parses the
 * pending-send snapshot and attaches the dev bearer token.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import { cancelPendingSend, getPendingSendStatus } from "./undoSend";
import { DEV_BEARER_TOKEN } from "./jmap";

function mockFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn();
  for (const r of responses) fetchMock.mockResolvedValueOnce(r);
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("cancelPendingSend", () => {
  it("POSTs to the cancel endpoint and resolves cancelled:true on 200", async () => {
    const fetchMock = mockFetch(new Response(null, { status: 200 }));
    await expect(cancelPendingSend("send 1")).resolves.toEqual({
      cancelled: true,
    });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/send/send%201/cancel");
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${DEV_BEARER_TOKEN}`,
    );
  });

  it("resolves cancelled:false when the worker already dispatched (410)", async () => {
    mockFetch(new Response("gone", { status: 410 }));
    await expect(cancelPendingSend("s1")).resolves.toEqual({
      cancelled: false,
    });
  });

  it("throws on an unexpected status", async () => {
    mockFetch(new Response("boom", { status: 500 }));
    await expect(cancelPendingSend("s1")).rejects.toThrow(/cancelPendingSend: 500/);
  });
});

describe("getPendingSendStatus", () => {
  it("returns the parsed snapshot", async () => {
    const snap = {
      id: "s1",
      status: "pending",
      email_id: "e1",
      created_at: "2026-01-01T00:00:00Z",
      deadline_at: "2026-01-01T00:00:30Z",
      attempts: 0,
    };
    mockFetch(
      new Response(JSON.stringify(snap), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    await expect(getPendingSendStatus("s1")).resolves.toMatchObject({
      id: "s1",
      status: "pending",
    });
  });

  it("throws on a non-ok status", async () => {
    mockFetch(new Response("nope", { status: 404 }));
    await expect(getPendingSendStatus("s1")).rejects.toThrow(
      /getPendingSendStatus: 404/,
    );
  });
});
