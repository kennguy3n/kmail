/**
 * Public self-service signup client (gap-closure Session 3).
 *
 * These endpoints are intentionally unauthenticated — the signup
 * funnel runs before any tenant or user exists, so unlike the rest of
 * the REST client surface (see `api/admin.ts`) there are no bearer or
 * tenant headers. The matching server contract lives in
 * `internal/tenant/signup_handlers.go`.
 */

const BASE = "/api/v1";

/** SignupPlan mirrors the `signup_requests.plan` CHECK constraint. */
export type SignupPlan = "core" | "pro" | "privacy";

/** SignupStatus mirrors the `signup_requests.status` CHECK constraint. */
export type SignupStatus = "pending" | "active" | "failed" | "expired";

/** SignupPlanTier is a single plan card on the signup form. Mirrors
 * the server-side `PLAN_CATALOG` in `internal/tenant/signup.go`. */
export interface SignupPlanTier {
  id: SignupPlan;
  name: string;
  description: string;
  features: string[];
}

/**
 * PLAN_CATALOG is the marketing copy for the three self-service tiers.
 * It is duplicated from the server (`internal/tenant/signup.go`) so the
 * public signup page renders without an extra round-trip; the server
 * remains the source of truth and re-validates the chosen plan on
 * POST /api/v1/signup.
 */
export const PLAN_CATALOG: SignupPlanTier[] = [
  {
    id: "core",
    name: "Core",
    description: "Privacy-first email and calendar for small teams.",
    features: ["Encrypted mailboxes", "Custom domain", "Shared inboxes"],
  },
  {
    id: "pro",
    name: "Pro",
    description: "Advanced controls and higher quotas for growing businesses.",
    features: [
      "Everything in Core",
      "Higher storage quotas",
      "Confidential send + portals",
      "Priority deliverability",
    ],
  },
  {
    id: "privacy",
    name: "Privacy",
    description:
      "Zero-access vaults and client-side encryption for the privacy-obsessed.",
    features: [
      "Everything in Pro",
      "Zero-access (StrictZK) vaults",
      "Client-side encryption keys",
      "Customer-managed key (CMK) support",
    ],
  },
];

/** SignupRequest mirrors the JSON returned by the signup endpoints. */
export interface SignupRequest {
  id: string;
  email: string;
  org_name: string;
  plan: SignupPlan;
  status: SignupStatus;
  stripe_checkout_session_id?: string;
  checkout_url?: string;
  created_at: string;
  completed_at?: string;
}

/** SignupApiError carries the URL, HTTP status, and parsed server
 * message for a failed signup request. */
export class SignupApiError extends Error {
  constructor(
    readonly url: string,
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "SignupApiError";
  }
}

async function parseError(url: string, res: Response): Promise<SignupApiError> {
  const text = await res.text();
  let message = text;
  try {
    const body = JSON.parse(text) as { error?: string };
    if (body.error) {
      message = body.error;
    }
  } catch {
    // Non-JSON body — fall back to the raw text.
  }
  return new SignupApiError(url, res.status, message || `${res.status}`);
}

/**
 * initiateSignup creates a pending signup request and returns it with
 * the Stripe-hosted `checkout_url` the caller redirects to.
 */
export async function initiateSignup(
  email: string,
  orgName: string,
  plan: SignupPlan,
): Promise<SignupRequest> {
  const url = `${BASE}/signup`;
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, org_name: orgName, plan }),
  });
  if (!res.ok) {
    throw await parseError(url, res);
  }
  return (await res.json()) as SignupRequest;
}

/**
 * getSignupStatus polls the status of a signup request by id. Returns
 * the full request so callers can branch on `status`.
 */
export async function getSignupStatus(id: string): Promise<SignupRequest> {
  const url = `${BASE}/signup/${encodeURIComponent(id)}/status`;
  const res = await fetch(url);
  if (!res.ok) {
    throw await parseError(url, res);
  }
  return (await res.json()) as SignupRequest;
}
