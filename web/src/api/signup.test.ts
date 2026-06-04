/**
 * Smoke tests for `web/src/api/signup.ts`.
 *
 * The signup client speaks to the public, unauthenticated endpoints
 * in `internal/tenant/signup_handlers.go`. These tests pin:
 *
 *   1. Request shape — POST /api/v1/signup carries a JSON body with
 *      `email`, `org_name`, `plan` and NO auth headers (the funnel is
 *      pre-tenant).
 *   2. Error mapping — non-2xx responses surface as `SignupApiError`
 *      with the parsed `{ "error": "..." }` message.
 *   3. Status polling — GET /api/v1/signup/{id}/status round-trips the
 *      request and url-encodes the id.
 */
import { describe, expect, it, vi } from "vitest";

import {
  PLAN_CATALOG,
  SignupApiError,
  getSignupStatus,
  initiateSignup,
} from "./signup";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("initiateSignup", () => {
  it("POSTs email/org/plan as JSON without auth headers", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        jsonResponse(
          {
            id: "req-1",
            email: "a@acme.com",
            org_name: "Acme",
            plan: "pro",
            status: "pending",
            checkout_url: "https://checkout.stripe.test/1",
            created_at: "2024-01-01T00:00:00Z",
          },
          201,
        ),
      );

    const req = await initiateSignup("a@acme.com", "Acme", "pro");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/signup");
    expect(init?.method).toBe("POST");
    expect(JSON.parse(init?.body as string)).toEqual({
      email: "a@acme.com",
      org_name: "Acme",
      plan: "pro",
    });
    // Pre-auth: no bearer / tenant headers.
    const headers = init?.headers as Record<string, string>;
    expect(headers["Content-Type"]).toBe("application/json");
    expect(Object.keys(headers)).not.toContain("Authorization");
    expect(req.checkout_url).toBe("https://checkout.stripe.test/1");
  });

  it("maps a non-2xx response to SignupApiError with the server message", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      jsonResponse({ error: "plan must be one of core, pro, privacy" }, 400),
    );

    await expect(initiateSignup("a@acme.com", "Acme", "pro")).rejects.toThrow(
      SignupApiError,
    );
  });

  it("surfaces the parsed error message text", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      jsonResponse({ error: "rate limit exceeded" }, 429),
    );
    await expect(
      initiateSignup("a@acme.com", "Acme", "core"),
    ).rejects.toMatchObject({ status: 429, message: "rate limit exceeded" });
  });
});

describe("getSignupStatus", () => {
  it("GETs the status endpoint and url-encodes the id", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        // The status endpoint returns the minimal public projection
        // (no email / org_name / stripe session id).
        jsonResponse({
          id: "req-1",
          plan: "core",
          status: "active",
          created_at: "2024-01-01T00:00:00Z",
        }),
      );

    const req = await getSignupStatus("req 1/x");
    expect(fetchMock).toHaveBeenCalledWith("/api/v1/signup/req%201%2Fx/status");
    expect(req.status).toBe("active");
  });

  it("maps a 404 to SignupApiError", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      jsonResponse({ error: "not found" }, 404),
    );
    await expect(getSignupStatus("missing")).rejects.toMatchObject({
      status: 404,
    });
  });
});

describe("PLAN_CATALOG", () => {
  it("mirrors the server's three tiers in order", () => {
    expect(PLAN_CATALOG.map((p) => p.id)).toEqual(["core", "pro", "privacy"]);
    for (const tier of PLAN_CATALOG) {
      expect(tier.features.length).toBeGreaterThan(0);
    }
  });
});
