/**
 * Unit tests for the Push Service client.
 *
 * Covers the subscribe/list/preferences REST wrappers (URL + method
 * + admin auth + envelope unwrapping) and the guard rails in
 * `registerWebPush`: it must reject when the Notifications API is
 * unavailable and when the user denies permission, before ever
 * touching the push manager.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  getPreferences,
  listSubscriptions,
  registerWebPush,
  subscribe,
  updatePreferences,
} from "./push";
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
  vi.unstubAllGlobals();
});

describe("subscribe", () => {
  it("POSTs the subscription payload with admin auth", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "sub-1" }));
    await subscribe({ device_type: "web", push_endpoint: "https://push/x" });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/push/subscribe");
    expect(init.method).toBe("POST");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${DEV_BEARER_TOKEN}`,
    );
    expect(JSON.parse(init.body as string)).toMatchObject({
      device_type: "web",
    });
  });
});

describe("listSubscriptions", () => {
  it("unwraps the subscriptions envelope", async () => {
    mockFetch(jsonResponse({ subscriptions: [{ id: "s1" }, { id: "s2" }] }));
    await expect(listSubscriptions()).resolves.toHaveLength(2);
  });

  it("tolerates a missing envelope", async () => {
    mockFetch(jsonResponse({}));
    await expect(listSubscriptions()).resolves.toEqual([]);
  });
});

describe("preferences", () => {
  it("GETs the preferences endpoint", async () => {
    const fetchMock = mockFetch(jsonResponse({ new_email: true }));
    await getPreferences();
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/push/preferences");
    expect((init.method ?? "GET")).toBe("GET");
  });

  it("PUTs a preferences patch", async () => {
    const fetchMock = mockFetch(jsonResponse({ new_email: false }));
    await updatePreferences({ new_email: false });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/push/preferences");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string)).toEqual({ new_email: false });
  });
});

describe("registerWebPush", () => {
  it("rejects when the Notifications API is unavailable", async () => {
    vi.stubGlobal("Notification", undefined);
    await expect(
      registerWebPush({} as ServiceWorkerRegistration, "key"),
    ).rejects.toThrow(/Notifications API unavailable/);
  });

  it("rejects when the user denies notification permission", async () => {
    vi.stubGlobal("Notification", {
      requestPermission: vi.fn().mockResolvedValue("denied"),
    });
    const registration = {
      pushManager: { subscribe: vi.fn() },
    } as unknown as ServiceWorkerRegistration;

    await expect(registerWebPush(registration, "key")).rejects.toThrow(
      /permission denied/i,
    );
    // Must not attempt to subscribe after a denial.
    expect(
      (registration.pushManager.subscribe as ReturnType<typeof vi.fn>),
    ).not.toHaveBeenCalled();
  });
});
