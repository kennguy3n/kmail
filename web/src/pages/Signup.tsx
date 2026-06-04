/**
 * Signup is the public, unauthenticated self-service signup funnel
 * (gap-closure Session 3). It is mounted outside the {@link Layout}
 * shell (like the Confidential Send portal) because the visitor has
 * no tenant or session yet.
 *
 * Three steps, driven by the URL so a Stripe round-trip survives a
 * full page navigation:
 *
 *   1. Form — email + org name + plan selector (cards from
 *      {@link PLAN_CATALOG}). Submitting POSTs to /api/v1/signup and
 *      redirects the browser to the returned Stripe Checkout URL.
 *   2. Stripe Checkout — hosted by Stripe; on success it redirects
 *      back to /signup?status=success&id=...&session_id=...; on
 *      cancel to /signup?status=cancelled&id=...
 *   3. Processing — polls /api/v1/signup/{id}/status every 2s until
 *      the webhook flips it to `active`, then forwards to the DNS
 *      wizard. Surfaces failed/expired payments.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import {
  PLAN_CATALOG,
  SignupApiError,
  getSignupStatus,
  initiateSignup,
  type SignupPlan,
} from "../api/signup";

/** Where a provisioned tenant is sent to finish setup. */
const POST_SIGNUP_PATH = "/admin/dns-wizard";

export interface SignupProps {
  /** Poll cadence for the processing step. Overridable for tests. */
  pollIntervalMs?: number;
  /** Browser redirect to Stripe Checkout. Overridable for tests.
   * Defaults to a full navigation so the Stripe-hosted page replaces
   * the SPA. */
  onRedirect?: (url: string) => void;
}

export default function Signup({
  pollIntervalMs = 2000,
  onRedirect = (url) => window.location.assign(url),
}: SignupProps) {
  const [params] = useSearchParams();
  const status = params.get("status");
  const idParam = params.get("id");

  if (status === "success" && idParam) {
    return (
      <SignupProcessing id={idParam} pollIntervalMs={pollIntervalMs} />
    );
  }
  if (status === "cancelled") {
    return <SignupCancelled />;
  }
  return <SignupForm onRedirect={onRedirect} />;
}

// --- Step 1: form --------------------------------------------------

function SignupForm({ onRedirect }: { onRedirect: (url: string) => void }) {
  const [email, setEmail] = useState("");
  const [orgName, setOrgName] = useState("");
  const [plan, setPlan] = useState<SignupPlan>("pro");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      setError(null);

      const trimmedEmail = email.trim();
      const trimmedOrg = orgName.trim();
      if (!trimmedEmail || !trimmedOrg) {
        setError("Email and organization name are required.");
        return;
      }

      setSubmitting(true);
      try {
        const req = await initiateSignup(trimmedEmail, trimmedOrg, plan);
        if (!req.checkout_url) {
          throw new Error("No checkout URL was returned.");
        }
        onRedirect(req.checkout_url);
      } catch (err) {
        const message =
          err instanceof SignupApiError
            ? err.message
            : err instanceof Error
              ? err.message
              : "Something went wrong. Please try again.";
        setError(message);
        setSubmitting(false);
      }
    },
    [email, orgName, plan, onRedirect],
  );

  return (
    <main style={styles.page}>
      <div style={styles.card}>
        <h1 style={styles.h1}>Create your KMail workspace</h1>
        <p style={styles.subtitle}>
          Private, encrypted email and calendar for your team. Start your
          subscription in minutes.
        </p>

        <form onSubmit={onSubmit} noValidate>
          <label style={styles.label} htmlFor="signup-email">
            Work email
          </label>
          <input
            id="signup-email"
            type="email"
            autoComplete="email"
            style={styles.input}
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="you@yourcompany.com"
            required
          />

          <label style={styles.label} htmlFor="signup-org">
            Organization name
          </label>
          <input
            id="signup-org"
            type="text"
            style={styles.input}
            value={orgName}
            onChange={(e) => setOrgName(e.target.value)}
            placeholder="Acme Inc"
            required
          />

          <fieldset style={styles.fieldset}>
            <legend style={styles.label}>Choose a plan</legend>
            <div style={styles.planGrid} role="radiogroup" aria-label="Plan">
              {PLAN_CATALOG.map((tier) => {
                const selected = tier.id === plan;
                return (
                  <button
                    type="button"
                    key={tier.id}
                    role="radio"
                    aria-checked={selected}
                    onClick={() => setPlan(tier.id)}
                    style={{
                      ...styles.planCard,
                      ...(selected ? styles.planCardSelected : {}),
                    }}
                  >
                    <span style={styles.planName}>{tier.name}</span>
                    <span style={styles.planDesc}>{tier.description}</span>
                    <ul style={styles.featureList}>
                      {tier.features.map((f) => (
                        <li key={f}>{f}</li>
                      ))}
                    </ul>
                  </button>
                );
              })}
            </div>
          </fieldset>

          {error && (
            <p role="alert" style={styles.error}>
              {error}
            </p>
          )}

          <button type="submit" style={styles.submit} disabled={submitting}>
            {submitting ? "Redirecting to checkout…" : "Continue to payment"}
          </button>
        </form>
      </div>
    </main>
  );
}

// --- Step 3: processing --------------------------------------------

function SignupProcessing({
  id,
  pollIntervalMs,
}: {
  id: string;
  pollIntervalMs: number;
}) {
  const navigate = useNavigate();
  const [phase, setPhase] = useState<"working" | "failed" | "expired">(
    "working",
  );
  const [error, setError] = useState<string | null>(null);
  // Guard against a late poll resolving after we've navigated away.
  const doneRef = useRef(false);

  useEffect(() => {
    doneRef.current = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const poll = async () => {
      if (doneRef.current) {
        return;
      }
      try {
        const req = await getSignupStatus(id);
        if (doneRef.current) {
          return;
        }
        switch (req.status) {
          case "active":
            doneRef.current = true;
            navigate(POST_SIGNUP_PATH, { replace: true });
            return;
          case "failed":
            doneRef.current = true;
            setPhase("failed");
            return;
          case "expired":
            doneRef.current = true;
            setPhase("expired");
            return;
          default:
            // still pending — schedule the next poll.
            timer = setTimeout(poll, pollIntervalMs);
        }
      } catch (err) {
        if (doneRef.current) {
          return;
        }
        // Transient fetch/5xx errors shouldn't abort polling — the
        // webhook may not have landed yet. Surface a soft message and
        // keep trying.
        setError(
          err instanceof Error ? err.message : "Temporary error while checking status.",
        );
        timer = setTimeout(poll, pollIntervalMs);
      }
    };

    void poll();
    return () => {
      doneRef.current = true;
      if (timer) {
        clearTimeout(timer);
      }
    };
  }, [id, pollIntervalMs, navigate]);

  return (
    <main style={styles.page}>
      <div style={styles.card}>
        {phase === "working" && (
          <>
            <h1 style={styles.h1}>Setting up your workspace…</h1>
            <p style={styles.subtitle}>
              Payment received. We&apos;re provisioning your tenant and
              creating your admin account. This usually takes a few seconds.
            </p>
            <div role="status" aria-live="polite" style={styles.spinner}>
              Working…
            </div>
            {error && (
              <p style={styles.softError}>
                Still working ({error}). Retrying…
              </p>
            )}
          </>
        )}
        {phase === "failed" && (
          <>
            <h1 style={styles.h1}>Payment didn&apos;t go through</h1>
            <p style={styles.subtitle}>
              We couldn&apos;t complete your subscription. No charge was made.
              You can try signing up again.
            </p>
            <a href="/signup" style={styles.linkButton}>
              Start over
            </a>
          </>
        )}
        {phase === "expired" && (
          <>
            <h1 style={styles.h1}>Your checkout session expired</h1>
            <p style={styles.subtitle}>
              The payment session timed out before it was completed. Please
              start a new signup.
            </p>
            <a href="/signup" style={styles.linkButton}>
              Start over
            </a>
          </>
        )}
      </div>
    </main>
  );
}

// --- Step 2b: cancelled --------------------------------------------

function SignupCancelled() {
  return (
    <main style={styles.page}>
      <div style={styles.card}>
        <h1 style={styles.h1}>Checkout cancelled</h1>
        <p style={styles.subtitle}>
          You cancelled before completing payment, so no workspace was created
          and you weren&apos;t charged.
        </p>
        <a href="/signup" style={styles.linkButton}>
          Back to signup
        </a>
      </div>
    </main>
  );
}

// --- styles --------------------------------------------------------

const styles: Record<string, React.CSSProperties> = {
  page: {
    minHeight: "100vh",
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    background: "#0f172a",
    padding: "2rem",
  },
  card: {
    width: "100%",
    maxWidth: 720,
    background: "#ffffff",
    borderRadius: 12,
    padding: "2.5rem",
    boxShadow: "0 10px 40px rgba(0,0,0,0.25)",
  },
  h1: { margin: "0 0 0.5rem", fontSize: "1.6rem", color: "#0f172a" },
  subtitle: { margin: "0 0 1.5rem", color: "#475569", lineHeight: 1.5 },
  label: {
    display: "block",
    fontWeight: 600,
    margin: "1rem 0 0.35rem",
    color: "#1e293b",
  },
  input: {
    width: "100%",
    padding: "0.65rem 0.75rem",
    borderRadius: 8,
    border: "1px solid #cbd5e1",
    fontSize: "1rem",
    boxSizing: "border-box",
  },
  fieldset: { border: "none", padding: 0, margin: "1.25rem 0 0" },
  planGrid: {
    display: "grid",
    gridTemplateColumns: "repeat(3, 1fr)",
    gap: "0.75rem",
  },
  planCard: {
    textAlign: "left",
    border: "2px solid #e2e8f0",
    borderRadius: 10,
    padding: "1rem",
    background: "#f8fafc",
    cursor: "pointer",
    display: "flex",
    flexDirection: "column",
    gap: "0.4rem",
  },
  planCardSelected: { borderColor: "#2563eb", background: "#eff6ff" },
  planName: { fontWeight: 700, fontSize: "1.1rem", color: "#0f172a" },
  planDesc: { fontSize: "0.85rem", color: "#475569" },
  featureList: {
    margin: "0.25rem 0 0",
    paddingLeft: "1.1rem",
    fontSize: "0.8rem",
    color: "#334155",
  },
  submit: {
    marginTop: "1.5rem",
    width: "100%",
    padding: "0.8rem",
    borderRadius: 8,
    border: "none",
    background: "#2563eb",
    color: "#fff",
    fontSize: "1rem",
    fontWeight: 600,
    cursor: "pointer",
  },
  error: {
    color: "#b91c1c",
    background: "#fef2f2",
    border: "1px solid #fecaca",
    borderRadius: 8,
    padding: "0.6rem 0.75rem",
    margin: "1rem 0 0",
  },
  softError: { color: "#92400e", marginTop: "1rem", fontSize: "0.9rem" },
  spinner: {
    marginTop: "1.5rem",
    fontSize: "1.1rem",
    color: "#2563eb",
    fontWeight: 600,
  },
  linkButton: {
    display: "inline-block",
    marginTop: "1.5rem",
    padding: "0.7rem 1.2rem",
    borderRadius: 8,
    background: "#2563eb",
    color: "#fff",
    textDecoration: "none",
    fontWeight: 600,
  },
};
