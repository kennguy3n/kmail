/**
 * Unit tests for the Confidential Send client.
 *
 * Two distinct auth regimes: the tenant-scoped create/list/revoke
 * routes carry the admin auth headers, while the public-portal
 * `getSecureMessage` lookup is intentionally unauthenticated (token
 * + optional password are the only credentials) and switches between
 * GET (probe) and POST (with password) based on the argument.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createSecureMessage,
  getSecureMessage,
  listSecureMessages,
} from "./confidentialSend";
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

describe("createSecureMessage", () => {
  it("POSTs the snake_cased body to the tenant-scoped endpoint with admin auth", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "sm-1", link_token: "tok" }));
    await createSecureMessage({
      tenantId: "tenant 1",
      senderId: "user-1",
      encryptedBlobRef: "blob-1",
      password: "hunter2",
      expiresInSeconds: 3600,
      maxViews: 3,
    });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/tenants/tenant%201/confidential-send");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${DEV_BEARER_TOKEN}`,
    );
    expect(JSON.parse(init.body as string)).toEqual({
      sender_id: "user-1",
      encrypted_blob_ref: "blob-1",
      password: "hunter2",
      expires_in_seconds: 3600,
      max_views: 3,
    });
  });
});

describe("listSecureMessages", () => {
  it("adds the sender_id query param when provided", async () => {
    const fetchMock = mockFetch(jsonResponse([{ id: "sm-1" }]));
    await listSecureMessages("t1", "sender 9");
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(
      "/api/v1/tenants/t1/confidential-send?sender_id=sender%209",
    );
  });

  it("omits the query param when no sender is given", async () => {
    const fetchMock = mockFetch(jsonResponse([]));
    await listSecureMessages("t1");
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/tenants/t1/confidential-send");
  });
});

describe("getSecureMessage (public portal)", () => {
  it("GETs without auth headers when no password is supplied", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "sm-1", has_password: true }));
    await getSecureMessage("link-token");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/secure/link-token");
    expect(init.method ?? "GET").toBe("GET");
    expect(new Headers(init.headers).get("Authorization")).toBeNull();
  });

  it("POSTs the password when one is supplied", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "sm-1" }));
    await getSecureMessage("link-token", "secret");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/secure/link-token");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ password: "secret" });
    expect(new Headers(init.headers).get("Authorization")).toBeNull();
  });

  it("maps a wrong-password response to AdminApiError", async () => {
    mockFetch(jsonResponse({ error: "invalid password" }, 403));
    await expect(getSecureMessage("tok", "bad")).rejects.toMatchObject({
      name: "AdminApiError",
      status: 403,
      message: expect.stringContaining("invalid password"),
    });
  });
});
