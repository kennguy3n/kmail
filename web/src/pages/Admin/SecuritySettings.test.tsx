/**
 * Page-level integration tests for the Security settings page.
 *
 * Covers the two management surfaces a tenant admin uses to harden an
 * account:
 *
 *   - WebAuthn tab: lists registered keys, issues a registration
 *     challenge ("Register a new key"), and removes a key.
 *   - TOTP tab: the full enrol → verify → recovery-codes HTTP flow.
 *
 * The WebAuthn list/delete go through mocked admin functions; the
 * registration-challenge and TOTP endpoints are raw `fetch` calls, so
 * a routing `fetch` stub serves them. Queries are by role / label so
 * the assertions survive the design-system migration.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import SecuritySettings from "./SecuritySettings";
import type { Tenant, WebAuthnCredential } from "../../api/admin";

const listTenants = vi.fn();
const listWebAuthnCredentials = vi.fn();
const deleteWebAuthnCredential = vi.fn();

vi.mock("../../api/admin", async () => {
  const actual =
    await vi.importActual<typeof import("../../api/admin")>("../../api/admin");
  return {
    ...actual,
    listTenants: (...args: unknown[]) => listTenants(...args),
    listWebAuthnCredentials: (...args: unknown[]) =>
      listWebAuthnCredentials(...args),
    deleteWebAuthnCredential: (...args: unknown[]) =>
      deleteWebAuthnCredential(...args),
  };
});

const tenant: Tenant = {
  id: "tenant-1",
  name: "Acme",
  slug: "acme",
  plan: "core",
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const credential: WebAuthnCredential = {
  id: "cred-1",
  credential_id: "raw-1",
  name: "YubiKey 5C",
  created_at: "2026-01-01T00:00:00Z",
  last_used_at: null,
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/**
 * Routes the component's raw `fetch` calls (WebAuthn begin + TOTP
 * lifecycle). Tests pass per-endpoint overrides as needed.
 */
function routeFetch(overrides: Record<string, () => Response> = {}): void {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = (init?.method ?? "GET").toUpperCase();
      const key = `${method} ${url}`;
      if (overrides[key]) return Promise.resolve(overrides[key]());
      if (url.endsWith("/auth/totp/status")) {
        return Promise.resolve(jsonResponse({ enrolled: false, enabled: false }));
      }
      return Promise.reject(new Error(`unrouted fetch: ${key}`));
    }),
  );
}

afterEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
});

async function selectTenant(): Promise<void> {
  const tenantSelect = await screen.findByRole("combobox", { name: /tenant/i });
  await waitFor(() =>
    expect(
      within(tenantSelect).getByRole("option", { name: "Acme" }),
    ).toBeInTheDocument(),
  );
  await userEvent.selectOptions(tenantSelect, "tenant-1");
}

describe("<SecuritySettings /> WebAuthn", () => {
  it("lists registered security keys for the selected tenant", async () => {
    listTenants.mockResolvedValue([tenant]);
    listWebAuthnCredentials.mockResolvedValue({ credentials: [credential] });
    routeFetch();

    render(<SecuritySettings />);
    await selectTenant();

    expect(
      await screen.findByRole("cell", { name: "YubiKey 5C" }),
    ).toBeInTheDocument();
    expect(listWebAuthnCredentials).toHaveBeenCalledWith("tenant-1");
  });

  it("issues a registration challenge when adding a key", async () => {
    listTenants.mockResolvedValue([tenant]);
    listWebAuthnCredentials.mockResolvedValue({ credentials: [] });
    routeFetch({
      "POST /api/v1/auth/webauthn/register/begin": () =>
        jsonResponse({ rp: { id: "kmail" }, challenge: "abc" }),
    });
    // The page guards on navigator.credentials before reporting success.
    Object.defineProperty(navigator, "credentials", {
      value: {},
      configurable: true,
    });

    render(<SecuritySettings />);
    await selectTenant();
    await userEvent.click(
      screen.getByRole("button", { name: "Register a new key" }),
    );

    expect(
      await screen.findByText(/Registration challenge issued/),
    ).toBeInTheDocument();
  });

  it("removes a credential and refreshes the list", async () => {
    listTenants.mockResolvedValue([tenant]);
    listWebAuthnCredentials
      .mockResolvedValueOnce({ credentials: [credential] })
      .mockResolvedValueOnce({ credentials: [] });
    deleteWebAuthnCredential.mockResolvedValue(undefined);
    routeFetch();

    render(<SecuritySettings />);
    await selectTenant();
    await screen.findByRole("cell", { name: "YubiKey 5C" });

    await userEvent.click(screen.getByRole("button", { name: "Remove" }));

    await waitFor(() =>
      expect(deleteWebAuthnCredential).toHaveBeenCalledWith("tenant-1", "cred-1"),
    );
    await waitFor(() =>
      expect(screen.queryByRole("cell", { name: "YubiKey 5C" })).toBeNull(),
    );
  });
});

describe("<SecuritySettings /> TOTP", () => {
  it("runs the enrol → verify flow and surfaces recovery codes", async () => {
    listTenants.mockResolvedValue([tenant]);
    listWebAuthnCredentials.mockResolvedValue({ credentials: [] });
    routeFetch({
      "POST /api/v1/auth/totp/enroll": () =>
        jsonResponse({
          otpauth_uri: "otpauth://totp/kmail:alice?secret=JBSWY3DPEHPK3PXP",
          secret: "JBSWY3DPEHPK3PXP",
        }),
      "POST /api/v1/auth/totp/verify": () =>
        jsonResponse({ recovery_codes: ["aaaa-bbbb", "cccc-dddd"] }),
    });

    render(<SecuritySettings />);
    await selectTenant();

    // Switch to the TOTP tab.
    await userEvent.click(
      screen.getByRole("button", { name: /TOTP \(authenticator app\)/ }),
    );
    expect(await screen.findByText(/not enrolled/)).toBeInTheDocument();

    // Begin enrolment exposes the shared secret.
    await userEvent.click(
      screen.getByRole("button", { name: "Begin enrolment" }),
    );
    expect(await screen.findByText("JBSWY3DPEHPK3PXP")).toBeInTheDocument();

    // Verify is disabled until a 6-digit code is entered.
    const verifyBtn = screen.getByRole("button", { name: "Verify and enable" });
    expect(verifyBtn).toBeDisabled();
    await userEvent.type(screen.getByLabelText(/6-digit code/i), "123456");
    expect(verifyBtn).toBeEnabled();

    await userEvent.click(verifyBtn);

    // Recovery codes are shown after a successful verification.
    expect(
      await screen.findByRole("heading", { name: "Recovery codes" }),
    ).toBeInTheDocument();
    expect(screen.getByText("aaaa-bbbb")).toBeInTheDocument();
    expect(screen.getByText("cccc-dddd")).toBeInTheDocument();
  });
});
