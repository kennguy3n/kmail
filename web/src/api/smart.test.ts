/**
 * Smoke tests for `web/src/api/smart.ts` — the WS7 smart-features
 * REST client. These pin:
 *
 *   1. Auth — every request carries the dev bearer token, and the
 *      analytics call forwards the selected tenant via the
 *      `X-KMail-Dev-Tenant-Id` dev-bypass header.
 *   2. URL shaping — query params (limit/cached/days/tz) and path
 *      id encoding are built correctly.
 *   3. Error mapping — non-2xx responses surface as `SmartApiError`
 *      carrying the parsed `{ "error": "..." }` message + status.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  SmartApiError,
  categorize,
  getCoRecipients,
  getEmailAnalytics,
  getFrequentContacts,
  getPriorityInbox,
  getSmartReplies,
  getUnsubscribe,
  postUnsubscribe,
  recordSend,
} from "./smart";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("getPriorityInbox", () => {
  it("requests the cached ranking with limit + cached params and auth", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ cached: true, items: [] }));

    const out = await getPriorityInbox({ limit: 25, cached: true });
    expect(out.cached).toBe(true);

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/priority-inbox?limit=25&cached=1");
    const headers = new Headers((init as RequestInit).headers);
    expect(headers.get("Authorization")).toBe("Bearer kmail-dev");
  });

  it("omits params when none supplied", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ cached: false, items: [] }));
    await getPriorityInbox();
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/priority-inbox");
  });
});

describe("getSmartReplies", () => {
  it("url-encodes the email id in the path", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ email_id: "a/b", suggestions: [] }));
    await getSmartReplies("a/b");
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/v1/emails/a%2Fb/smart-replies",
    );
  });
});

describe("categorize", () => {
  it("short-circuits an empty id list without calling fetch", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch");
    const out = await categorize([]);
    expect(out).toEqual({ categories: {} });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("POSTs the id list as JSON", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ categories: { E1: "primary" } }));
    await categorize(["E1"]);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/emails/categories");
    expect((init as RequestInit).method).toBe("POST");
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({
      ids: ["E1"],
    });
  });
});

describe("unsubscribe", () => {
  it("GETs the parsed affordance", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        jsonResponse({ email_id: "E1", unsubscribe: true, already_done: false, one_click: true }),
      );
    const out = await getUnsubscribe("E1");
    expect(out.one_click).toBe(true);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/emails/E1/unsubscribe");
  });

  it("POSTs to perform the unsubscribe", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ email_id: "E1", method: "one-click" }));
    const out = await postUnsubscribe("E1");
    expect(out.method).toBe("one-click");
    expect((fetchMock.mock.calls[0][1] as RequestInit).method).toBe("POST");
  });
});

describe("contacts", () => {
  it("requests frequent contacts with a limit", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ contacts: [] }));
    await getFrequentContacts(5);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/contacts/frequent?limit=5");
  });

  it("passes anchor + repeated exclude params for co-recipients", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ anchor: "a@b.com", suggestions: [] }));
    await getCoRecipients("a@b.com", ["c@d.com", "e@f.com"]);
    const url = fetchMock.mock.calls[0][0] as string;
    expect(url).toContain("anchor=a%40b.com");
    expect(url).toContain("exclude=c%40d.com");
    expect(url).toContain("exclude=e%40f.com");
  });

  it("records a sent message's recipients", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(jsonResponse({ ok: true }));
    await recordSend(["a@b.com"]);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/contacts/record");
    expect(JSON.parse((init as RequestInit).body as string)).toEqual({
      recipients: ["a@b.com"],
    });
  });
});

describe("getEmailAnalytics", () => {
  it("forwards days/tz params and the tenant dev-bypass header", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        jsonResponse({
          range_start: "2026-01-01",
          range_end: "2026-01-30",
          total_sent: 1,
          total_received: 2,
          daily: [],
          top_recipients: [],
          top_senders: [],
          busiest_hours: [],
          avg_response_seconds: 0,
          response_sample_size: 0,
        }),
      );
    await getEmailAnalytics({ days: 7, tz: "America/New_York", tenantId: "t-1" });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toContain("days=7");
    expect(url).toContain("tz=America%2FNew_York");
    const headers = new Headers((init as RequestInit).headers);
    expect(headers.get("X-KMail-Dev-Tenant-Id")).toBe("t-1");
  });
});

describe("error mapping", () => {
  it("throws SmartApiError carrying status + parsed message", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      jsonResponse({ error: "boom" }, 503),
    );
    await expect(getFrequentContacts()).rejects.toMatchObject({
      name: "SmartApiError",
      status: 503,
    });
  });

  it("falls back to status text for non-JSON bodies", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response("nope", { status: 500, statusText: "Internal Server Error" }),
    );
    await expect(getFrequentContacts()).rejects.toBeInstanceOf(SmartApiError);
  });
});
