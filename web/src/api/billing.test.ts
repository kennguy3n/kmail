/**
 * Unit tests for the published-pricing billing client.
 *
 * `billing.ts` re-exports a couple of admin helpers and adds the
 * proration-preview and billing-history reads. These tests pin the
 * URL/scoping (the tenant id is URL-encoded and the `X-Kmail-Tenant`
 * dev header is attached) and the AdminApiError mapping on non-2xx.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  AdminApiError,
  getBillingHistory,
  getProrationPreview,
} from "./billing";

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

describe("getProrationPreview", () => {
  it("GETs the proration-preview endpoint with the plan query and tenant header", async () => {
    const fetchMock = mockFetch(
      jsonResponse({ tenant_id: "t 1", new_plan: "pro", proration_cents: 1234 }),
    );

    const preview = await getProrationPreview("t 1", "pro");

    expect(preview.proration_cents).toBe(1234);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/tenants/t%201/billing/proration-preview?plan=pro");
    const headers = new Headers(init.headers);
    expect(headers.get("X-Kmail-Tenant")).toBe("t 1");
  });

  it("throws AdminApiError on a non-2xx response", async () => {
    mockFetch(new Response("nope", { status: 402 }));
    await expect(getProrationPreview("t1", "pro")).rejects.toBeInstanceOf(
      AdminApiError,
    );
  });
});

describe("getBillingHistory", () => {
  it("returns the parsed history array", async () => {
    const fetchMock = mockFetch(
      jsonResponse([
        {
          id: "evt-1",
          event_type: "plan_change",
          amount_cents: 0,
          seat_count: 5,
          metadata: "{}",
          created_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );

    const history = await getBillingHistory("tenant-1");
    expect(history).toHaveLength(1);
    expect(history[0].event_type).toBe("plan_change");
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/tenants/tenant-1/billing/history");
  });

  it("maps a server error to AdminApiError carrying the status", async () => {
    mockFetch(new Response("boom", { status: 500 }));
    await expect(getBillingHistory("tenant-1")).rejects.toMatchObject({
      name: "AdminApiError",
      status: 500,
    });
  });
});
