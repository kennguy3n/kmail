/**
 * Smoke tests for `web/src/api/admin.ts`.
 *
 * The admin client carries the entire control-plane REST surface
 * (tenant CRUD, audit log, billing, DMARC, DNS wizard, migrations,
 * and friends — see internal/tenant/service.go and
 * internal/audit/handlers.go for the matching server contracts).
 *
 * These tests pin three load-bearing properties of the client:
 *
 *   1. **Auth header wiring** — every request must carry the dev
 *      bearer token, and tenant-scoped requests must also carry
 *      `X-KMail-Dev-Tenant-Id` so the BFF's
 *      `devClaimsFromHeaders` can resolve the right tenant.
 *   2. **Error mapping** — non-2xx responses surface as
 *      `AdminApiError` with the parsed `{ "error": "..." }` body
 *      so callers don't lose the server-side message.
 *   3. **Envelope unwrapping** — handlers like `getAuditLog` wrap
 *      rows in a `{ "entries": [...] }` envelope and the helper
 *      must unwrap them.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ADMIN_API_BASE,
  AdminApiError,
  adminAuthHeaders,
  exportAuditLog,
  getAuditLog,
  listTenants,
  listUsers,
  requestJSON,
  verifyAuditChain,
} from "./admin";
import { DEV_BEARER_TOKEN } from "./jmap";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function textResponse(text: string, status = 200): Response {
  return new Response(text, { status });
}

function mockFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn();
  for (const r of responses) {
    fetchMock.mockResolvedValueOnce(r);
  }
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("adminAuthHeaders", () => {
  it("sets the dev bearer token on every request", () => {
    const h = adminAuthHeaders();
    expect(h.get("Authorization")).toBe(`Bearer ${DEV_BEARER_TOKEN}`);
    expect(h.get("X-KMail-Dev-Tenant-Id")).toBeNull();
  });

  it("adds X-KMail-Dev-Tenant-Id when a tenantId is supplied", () => {
    const h = adminAuthHeaders("tenant-1");
    expect(h.get("X-KMail-Dev-Tenant-Id")).toBe("tenant-1");
  });

  it("merges extra headers without dropping auth", () => {
    const h = adminAuthHeaders("tenant-1", { Accept: "application/json" });
    expect(h.get("Accept")).toBe("application/json");
    expect(h.get("Authorization")).toBe(`Bearer ${DEV_BEARER_TOKEN}`);
  });
});

describe("requestJSON", () => {
  it("parses JSON for 2xx responses", async () => {
    mockFetch(jsonResponse({ ok: true }));
    const got = await requestJSON<{ ok: boolean }>("/example", {
      method: "GET",
    });
    expect(got).toEqual({ ok: true });
  });

  it("returns undefined for 204 No Content", async () => {
    // Response constructor rejects bodies on 204; pass null explicitly.
    mockFetch(new Response(null, { status: 204 }));
    const got = await requestJSON<void>("/example", { method: "DELETE" });
    expect(got).toBeUndefined();
  });

  it("throws AdminApiError with the parsed { error } body", async () => {
    mockFetch(jsonResponse({ error: "tenant not found" }, 404));
    await expect(
      requestJSON("/example", { method: "GET" }),
    ).rejects.toMatchObject({
      name: "AdminApiError",
      status: 404,
      message: expect.stringContaining("tenant not found"),
    });
  });

  it("falls back to the raw body when there's no { error } field", async () => {
    mockFetch(textResponse("internal boom", 500));
    await expect(
      requestJSON("/example", { method: "GET" }),
    ).rejects.toMatchObject({
      status: 500,
      message: expect.stringContaining("internal boom"),
    });
  });
});

describe("listTenants", () => {
  it("GETs /api/v1/tenants without a tenant header (admin scope)", async () => {
    const fetchMock = mockFetch(
      jsonResponse([{ id: "t-1", name: "Acme", slug: "acme" }]),
    );

    const tenants = await listTenants();

    expect(tenants).toHaveLength(1);
    expect(tenants[0].id).toBe("t-1");
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ADMIN_API_BASE}/tenants`);
    expect((init as RequestInit).method).toBe("GET");
    const headers = new Headers(init.headers);
    expect(headers.get("Authorization")).toBe(`Bearer ${DEV_BEARER_TOKEN}`);
    expect(headers.get("X-KMail-Dev-Tenant-Id")).toBeNull();
  });
});

describe("listUsers", () => {
  it("scopes the request to the tenant via header + URL segment", async () => {
    const fetchMock = mockFetch(jsonResponse([]));

    await listUsers("tenant-1");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ADMIN_API_BASE}/tenants/tenant-1/users`);
    const headers = new Headers(init.headers);
    expect(headers.get("X-KMail-Dev-Tenant-Id")).toBe("tenant-1");
  });

  it("URL-encodes the tenant id so a slash in the id can't escape the path", async () => {
    const fetchMock = mockFetch(jsonResponse([]));

    await listUsers("tenant/1");

    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ADMIN_API_BASE}/tenants/tenant%2F1/users`);
  });
});

describe("getAuditLog", () => {
  it("unwraps the { entries: [...] } envelope", async () => {
    mockFetch(
      jsonResponse({
        entries: [
          {
            id: "a-1",
            tenant_id: "tenant-1",
            actor_id: "u-1",
            actor_type: "admin",
            action: "tenant.create",
            resource_type: "tenant",
            resource_id: "tenant-1",
            metadata: null,
            ip_address: "127.0.0.1",
            user_agent: "test",
            prev_hash: "",
            entry_hash: "abc",
            created_at: new Date().toISOString(),
          },
        ],
      }),
    );

    const entries = await getAuditLog("tenant-1");

    expect(entries).toHaveLength(1);
    expect(entries[0].action).toBe("tenant.create");
  });

  it("appends provided filters as query parameters", async () => {
    const fetchMock = mockFetch(jsonResponse({ entries: [] }));

    await getAuditLog("tenant-1", {
      action: "tenant.update",
      limit: 50,
      offset: 100,
    });

    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/audit-log?");
    expect(url).toContain("action=tenant.update");
    expect(url).toContain("limit=50");
    expect(url).toContain("offset=100");
  });

  it("omits the query string when no filters are supplied", async () => {
    const fetchMock = mockFetch(jsonResponse({ entries: [] }));

    await getAuditLog("tenant-1");

    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ADMIN_API_BASE}/tenants/tenant-1/audit-log`);
  });

  it("returns [] when the server returns an empty envelope", async () => {
    mockFetch(jsonResponse({}));

    const entries = await getAuditLog("tenant-1");

    expect(entries).toEqual([]);
  });
});

describe("exportAuditLog", () => {
  it("returns the raw response body so callers can stream a download", async () => {
    mockFetch(textResponse("id,actor,action\n1,u-1,tenant.create\n", 200));

    const csv = await exportAuditLog("tenant-1", "csv");

    expect(csv).toContain("tenant.create");
  });
});

describe("verifyAuditChain", () => {
  it("returns { ok: true } on a successful verify", async () => {
    mockFetch(jsonResponse({}, 200));
    expect(await verifyAuditChain("tenant-1")).toEqual({ ok: true });
  });

  it("turns a 409 chain-broken response into { ok: false, error }", async () => {
    mockFetch(jsonResponse({ error: "hash mismatch at id 7" }, 409));
    expect(await verifyAuditChain("tenant-1")).toEqual({
      ok: false,
      error: "hash mismatch at id 7",
    });
  });

  it("rethrows non-409 failures as AdminApiError", async () => {
    mockFetch(textResponse("nope", 500));
    await expect(verifyAuditChain("tenant-1")).rejects.toBeInstanceOf(
      AdminApiError,
    );
  });
});
