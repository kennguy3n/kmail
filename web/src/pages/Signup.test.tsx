/**
 * Component test for Signup.tsx.
 *
 * Covers the three URL-driven steps of the self-service funnel:
 *
 *   1. Form — renders the three plan cards, and submitting initiates a
 *      signup and redirects the browser to the returned Stripe
 *      Checkout URL.
 *   2. Processing (?status=success&id=...) — polls the status endpoint
 *      and navigates to the DNS wizard once the tenant is `active`.
 *   3. Failure — a `failed` status surfaces an error + "start over".
 *
 * The Stripe redirect is injected via the `onRedirect` prop so the
 * test never touches `window.location`. Network calls go through the
 * `signup` API module, which is mocked.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import Signup from "./Signup";
import * as signupApi from "../api/signup";

vi.mock("../api/signup", async () => {
  const actual = await vi.importActual<typeof import("../api/signup")>(
    "../api/signup",
  );
  return {
    ...actual,
    initiateSignup: vi.fn(),
    getSignupStatus: vi.fn(),
  };
});

function renderAt(path: string, onRedirect = vi.fn()) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/signup" element={<Signup pollIntervalMs={10} onRedirect={onRedirect} />} />
        <Route path="/admin/dns-wizard" element={<div>DNS WIZARD</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("Signup form", () => {
  it("renders the three plan cards", () => {
    renderAt("/signup");
    expect(screen.getByText("Core")).toBeInTheDocument();
    expect(screen.getByText("Pro")).toBeInTheDocument();
    expect(screen.getByText("Privacy")).toBeInTheDocument();
  });

  it("initiates a signup and redirects to the checkout URL", async () => {
    const onRedirect = vi.fn();
    vi.mocked(signupApi.initiateSignup).mockResolvedValueOnce({
      id: "req-1",
      email: "a@acme.com",
      org_name: "Acme",
      plan: "pro",
      status: "pending",
      checkout_url: "https://checkout.stripe.test/1",
      created_at: "2024-01-01T00:00:00Z",
    });

    renderAt("/signup", onRedirect);

    await userEvent.type(screen.getByLabelText("Work email"), "a@acme.com");
    await userEvent.type(
      screen.getByLabelText("Organization name"),
      "Acme",
    );
    await userEvent.click(
      screen.getByRole("button", { name: /continue to payment/i }),
    );

    await waitFor(() => {
      expect(signupApi.initiateSignup).toHaveBeenCalledWith(
        "a@acme.com",
        "Acme",
        "pro",
      );
      expect(onRedirect).toHaveBeenCalledWith("https://checkout.stripe.test/1");
    });
  });

  it("shows a validation error when fields are empty", async () => {
    renderAt("/signup");
    await userEvent.click(
      screen.getByRole("button", { name: /continue to payment/i }),
    );
    expect(
      await screen.findByText(/email and organization name are required/i),
    ).toBeInTheDocument();
    expect(signupApi.initiateSignup).not.toHaveBeenCalled();
  });

  it("surfaces an API error from initiateSignup", async () => {
    vi.mocked(signupApi.initiateSignup).mockRejectedValueOnce(
      new signupApi.SignupApiError("/api/v1/signup", 503, "checkout unavailable"),
    );
    renderAt("/signup");
    await userEvent.type(screen.getByLabelText("Work email"), "a@acme.com");
    await userEvent.type(screen.getByLabelText("Organization name"), "Acme");
    await userEvent.click(
      screen.getByRole("button", { name: /continue to payment/i }),
    );
    expect(
      await screen.findByText(/checkout unavailable/i),
    ).toBeInTheDocument();
  });
});

describe("Signup processing", () => {
  it("polls status and redirects to the DNS wizard when active", async () => {
    vi.mocked(signupApi.getSignupStatus)
      .mockResolvedValueOnce({
        id: "req-1",
        plan: "pro",
        status: "pending",
        created_at: "2024-01-01T00:00:00Z",
      })
      .mockResolvedValueOnce({
        id: "req-1",
        plan: "pro",
        status: "active",
        created_at: "2024-01-01T00:00:00Z",
      });

    renderAt("/signup?status=success&id=req-1");

    expect(
      screen.getByText(/setting up your workspace/i),
    ).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByText("DNS WIZARD")).toBeInTheDocument();
    });
    expect(signupApi.getSignupStatus).toHaveBeenCalledWith("req-1");
  });

  it("clears the transient error message once polling recovers", async () => {
    vi.mocked(signupApi.getSignupStatus)
      // First poll fails transiently → soft amber message appears.
      .mockRejectedValueOnce(
        new signupApi.SignupApiError("/api/v1/signup/req-1/status", 503, "upstream unavailable"),
      )
      // Recovery: a successful poll (still pending) must clear the error.
      .mockResolvedValueOnce({
        id: "req-1",
        plan: "pro",
        status: "pending",
        created_at: "2024-01-01T00:00:00Z",
      })
      .mockResolvedValueOnce({
        id: "req-1",
        plan: "pro",
        status: "active",
        created_at: "2024-01-01T00:00:00Z",
      });

    renderAt("/signup?status=success&id=req-1");

    // The soft error surfaces after the first (failed) poll.
    expect(await screen.findByText(/still working .*retrying/i)).toBeInTheDocument();

    // Once a poll succeeds the amber message must disappear, and the
    // flow continues to the DNS wizard on active.
    await waitFor(() => {
      expect(screen.queryByText(/still working .*retrying/i)).not.toBeInTheDocument();
    });
    await waitFor(() => {
      expect(screen.getByText("DNS WIZARD")).toBeInTheDocument();
    });
  });

  it("shows a failure message when the signup failed", async () => {
    vi.mocked(signupApi.getSignupStatus).mockResolvedValueOnce({
      id: "req-1",
      plan: "pro",
      status: "failed",
      created_at: "2024-01-01T00:00:00Z",
    });

    renderAt("/signup?status=success&id=req-1");

    expect(
      await screen.findByText(/payment didn.t go through/i),
    ).toBeInTheDocument();
  });
});

describe("Signup cancelled", () => {
  it("shows the cancelled message", () => {
    renderAt("/signup?status=cancelled&id=req-1");
    expect(screen.getByText(/checkout cancelled/i)).toBeInTheDocument();
  });
});
