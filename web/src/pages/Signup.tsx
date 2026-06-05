/**
 * Signup is the public, unauthenticated self-service signup funnel
 * (gap-closure Session 3). It is mounted outside the {@link Layout}
 * shell (like the Confidential Send portal) because the visitor has
 * no tenant or session yet.
 *
 * The funnel is URL-driven so a Stripe round-trip survives a full
 * page navigation:
 *
 *   1. Form — a guided, multi-step wizard (account email → company +
 *      domain → plan → review). All steps are client-side; only the
 *      final "Continue to payment" submit POSTs to /api/v1/signup and
 *      redirects the browser to the returned Stripe Checkout URL. The
 *      wizard collects the same fields the server contract accepts
 *      (email, org_name, plan); the optional domain is stashed in
 *      sessionStorage to pre-fill the post-signup DNS wizard.
 *   2. Stripe Checkout — hosted by Stripe; on success it redirects
 *      back to /signup?status=success&id=...&session_id=...; on
 *      cancel to /signup?status=cancelled&id=...
 *   3. Processing — polls /api/v1/signup/{id}/status every 2s until
 *      the webhook flips it to `active`, then forwards to the DNS
 *      wizard. Surfaces failed/expired payments.
 *
 * Note (WS3, additive): the multi-step UX is purely front-end. The
 * server's POST /api/v1/signup contract is unchanged — it still takes
 * { email, org_name, plan }. Pre-payment email verification and
 * first-user creation are intentionally deferred (no backend endpoint
 * yet); verification + welcome email happen during post-payment
 * provisioning. See the PR description.
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

/** Default overall budget for the processing-step poll loop (10 min).
 * After this elapses without the webhook flipping the row to a terminal
 * status, we stop polling and surface a "taking longer than expected"
 * state instead of spinning forever. */
const DEFAULT_POLL_TIMEOUT_MS = 10 * 60 * 1000;

export interface SignupProps {
  /** Poll cadence for the processing step. Overridable for tests. */
  pollIntervalMs?: number;
  /** Overall budget for the processing-step poll loop before giving up
   * and showing the timeout state. Overridable for tests. */
  pollTimeoutMs?: number;
  /** Browser redirect to Stripe Checkout. Overridable for tests.
   * Defaults to a full navigation so the Stripe-hosted page replaces
   * the SPA. */
  onRedirect?: (url: string) => void;
}

export default function Signup({
  pollIntervalMs = 2000,
  pollTimeoutMs = DEFAULT_POLL_TIMEOUT_MS,
  onRedirect = (url) => window.location.assign(url),
}: SignupProps) {
  const [params] = useSearchParams();
  const status = params.get("status");
  const idParam = params.get("id");

  if (status === "success" && idParam) {
    return (
      <SignupProcessing
        id={idParam}
        pollIntervalMs={pollIntervalMs}
        pollTimeoutMs={pollTimeoutMs}
      />
    );
  }
  if (status === "cancelled") {
    return <SignupCancelled />;
  }
  return <SignupForm onRedirect={onRedirect} />;
}

// --- Step 1: multi-step form ---------------------------------------

/** sessionStorage key the post-signup DNS wizard can read to pre-fill
 * the domain the visitor entered during signup. Additive/forward-
 * compatible: nothing breaks if the wizard ignores it. */
const SIGNUP_DOMAIN_KEY = "kmail.signup.domain";

/** Ordered wizard steps. The labels render in the progress rail. */
const WIZARD_STEPS = ["Account", "Company", "Plan", "Review"] as const;
type WizardStep = 0 | 1 | 2 | 3;

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
// A permissive registrable-domain check (label.label, no scheme/path).
const DOMAIN_RE = /^(?=.{1,253}$)([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$/i;

function planById(id: SignupPlan) {
  return PLAN_CATALOG.find((p) => p.id === id) ?? PLAN_CATALOG[0];
}

function SignupForm({ onRedirect }: { onRedirect: (url: string) => void }) {
  const [step, setStep] = useState<WizardStep>(0);
  const [email, setEmail] = useState("");
  const [orgName, setOrgName] = useState("");
  const [domain, setDomain] = useState("");
  const [plan, setPlan] = useState<SignupPlan>("pro");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const go = useCallback((next: WizardStep) => {
    setError(null);
    setStep(next);
  }, []);

  // Per-step validation gate before advancing.
  const next = useCallback(() => {
    if (step === 0) {
      if (!EMAIL_RE.test(email.trim())) {
        setError("Enter a valid work email address.");
        return;
      }
      // Convenience: seed the domain from the email host so the
      // company step is pre-filled but still editable.
      if (!domain.trim()) {
        const host = email.trim().split("@")[1] ?? "";
        if (host) setDomain(host.toLowerCase());
      }
      go(1);
      return;
    }
    if (step === 1) {
      if (!orgName.trim()) {
        setError("Enter your organization name.");
        return;
      }
      if (domain.trim() && !DOMAIN_RE.test(domain.trim())) {
        setError("Enter a valid domain (e.g. acme.com), or leave it blank.");
        return;
      }
      go(2);
      return;
    }
    if (step === 2) {
      go(3);
    }
  }, [step, email, orgName, domain, go]);

  const onSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      setError(null);

      const trimmedEmail = email.trim();
      const trimmedOrg = orgName.trim();
      // Defensive: the wizard gates these, but guard the contract too.
      if (!EMAIL_RE.test(trimmedEmail) || !trimmedOrg) {
        setError("Email and organization name are required.");
        return;
      }

      setSubmitting(true);
      try {
        const req = await initiateSignup(trimmedEmail, trimmedOrg, plan);
        if (!req.checkout_url) {
          throw new Error("No checkout URL was returned.");
        }
        // Stash the entered domain so the post-provision DNS wizard can
        // pre-fill it. Best-effort: ignore storage failures (private
        // mode, disabled storage) — it's a convenience, not required.
        const trimmedDomain = domain.trim().toLowerCase();
        if (trimmedDomain) {
          try {
            window.sessionStorage.setItem(SIGNUP_DOMAIN_KEY, trimmedDomain);
          } catch {
            /* non-fatal */
          }
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
    [email, orgName, domain, plan, onRedirect],
  );

  const selectedPlan = planById(plan);

  return (
    <main style={styles.page}>
      <div style={styles.card}>
        <h1 style={styles.h1}>Create your KMail workspace</h1>
        <p style={styles.subtitle}>
          Private, encrypted email and calendar for your team. Start your
          subscription in minutes.
        </p>

        <Stepper current={step} />

        {/* Step 0 — account email */}
        {step === 0 && (
          <div>
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
              onKeyDown={(e) => e.key === "Enter" && next()}
              placeholder="you@yourcompany.com"
              required
            />
            <p style={styles.help}>
              We&apos;ll send a verification and welcome email here after
              checkout.
            </p>
            {error && (
              <p role="alert" style={styles.error}>
                {error}
              </p>
            )}
            <div style={styles.navRow}>
              <span />
              <button type="button" style={styles.submit} onClick={next}>
                Continue
              </button>
            </div>
          </div>
        )}

        {/* Step 1 — company + domain */}
        {step === 1 && (
          <div>
            <label style={styles.label} htmlFor="signup-org">
              Organization name
            </label>
            <input
              id="signup-org"
              type="text"
              style={styles.input}
              value={orgName}
              onChange={(e) => setOrgName(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && next()}
              placeholder="Acme Inc"
              required
            />

            <label style={styles.label} htmlFor="signup-domain">
              Email domain <span style={styles.optional}>(optional)</span>
            </label>
            <input
              id="signup-domain"
              type="text"
              autoComplete="off"
              style={styles.input}
              value={domain}
              onChange={(e) => setDomain(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && next()}
              placeholder="acme.com"
            />
            <p style={styles.help}>
              The domain you&apos;ll send and receive mail from. You can add or
              change this later in the DNS setup wizard.
            </p>

            {error && (
              <p role="alert" style={styles.error}>
                {error}
              </p>
            )}
            <div style={styles.navRow}>
              <button type="button" style={styles.back} onClick={() => go(0)}>
                Back
              </button>
              <button type="button" style={styles.submit} onClick={next}>
                Continue
              </button>
            </div>
          </div>
        )}

        {/* Step 2 — plan */}
        {step === 2 && (
          <div>
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
            <div style={styles.navRow}>
              <button type="button" style={styles.back} onClick={() => go(1)}>
                Back
              </button>
              <button type="button" style={styles.submit} onClick={next}>
                Continue
              </button>
            </div>
          </div>
        )}

        {/* Step 3 — review + pay */}
        {step === 3 && (
          <form onSubmit={onSubmit} noValidate>
            <dl style={styles.review}>
              <ReviewRow label="Work email" value={email.trim()} onEdit={() => go(0)} />
              <ReviewRow label="Organization" value={orgName.trim()} onEdit={() => go(1)} />
              {domain.trim() && (
                <ReviewRow
                  label="Domain"
                  value={domain.trim().toLowerCase()}
                  onEdit={() => go(1)}
                />
              )}
              <ReviewRow
                label="Plan"
                value={selectedPlan.name}
                onEdit={() => go(2)}
              />
            </dl>
            <p style={styles.help}>
              You&apos;ll complete payment securely on Stripe. After checkout we
              provision your tenant and email you to verify and finish setup.
            </p>

            {error && (
              <p role="alert" style={styles.error}>
                {error}
              </p>
            )}
            <div style={styles.navRow}>
              <button
                type="button"
                style={styles.back}
                onClick={() => go(2)}
                disabled={submitting}
              >
                Back
              </button>
              <button type="submit" style={styles.submit} disabled={submitting}>
                {submitting ? "Redirecting to checkout…" : "Continue to payment"}
              </button>
            </div>
          </form>
        )}
      </div>
    </main>
  );
}

/** Stepper renders the horizontal progress rail for the wizard. */
function Stepper({ current }: { current: number }) {
  return (
    <ol style={styles.stepper} aria-label="Signup progress">
      {WIZARD_STEPS.map((label, i) => {
        const state =
          i < current ? "done" : i === current ? "current" : "todo";
        return (
          <li key={label} style={styles.step}>
            <span
              aria-current={state === "current" ? "step" : undefined}
              style={{
                ...styles.stepDot,
                ...(state === "current" ? styles.stepDotCurrent : {}),
                ...(state === "done" ? styles.stepDotDone : {}),
              }}
            >
              {i + 1}
            </span>
            <span style={styles.stepLabel}>{label}</span>
          </li>
        );
      })}
    </ol>
  );
}

/** ReviewRow is one editable summary line on the final review step. */
function ReviewRow({
  label,
  value,
  onEdit,
}: {
  label: string;
  value: string;
  onEdit: () => void;
}) {
  return (
    <div style={styles.reviewRow}>
      <dt style={styles.reviewLabel}>{label}</dt>
      <dd style={styles.reviewValue}>
        <span>{value}</span>
        <button type="button" style={styles.editLink} onClick={onEdit}>
          Edit
        </button>
      </dd>
    </div>
  );
}

// --- Step 3: processing --------------------------------------------

function SignupProcessing({
  id,
  pollIntervalMs,
  pollTimeoutMs,
}: {
  id: string;
  pollIntervalMs: number;
  pollTimeoutMs: number;
}) {
  const navigate = useNavigate();
  const [phase, setPhase] = useState<
    "working" | "failed" | "expired" | "timeout"
  >("working");
  const [error, setError] = useState<string | null>(null);
  // Guard against a late poll resolving after we've navigated away.
  const doneRef = useRef(false);

  useEffect(() => {
    doneRef.current = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    // Absolute budget for the whole loop. The webhook may never land
    // (misconfigured Stripe endpoint, dropped delivery); without this
    // the user would stare at "Setting up your workspace…" forever and
    // the transient-error path would retry indefinitely. Captured once
    // per effect run so it spans every poll, pending or errored.
    const deadline = Date.now() + pollTimeoutMs;

    // scheduleNext queues the next poll unless we've blown the overall
    // budget, in which case it stops and surfaces the timeout state.
    const scheduleNext = () => {
      if (Date.now() >= deadline) {
        doneRef.current = true;
        setPhase("timeout");
        return;
      }
      timer = setTimeout(poll, pollIntervalMs);
    };

    const poll = async () => {
      if (doneRef.current) {
        return;
      }
      try {
        const req = await getSignupStatus(id);
        if (doneRef.current) {
          return;
        }
        // A poll succeeded — clear any soft error left over from a
        // previous transient failure so the amber message doesn't linger
        // once status checks have recovered.
        setError(null);
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
            // still pending — schedule the next poll (or time out).
            scheduleNext();
        }
      } catch (err) {
        if (doneRef.current) {
          return;
        }
        // Transient fetch/5xx errors shouldn't abort polling — the
        // webhook may not have landed yet. Surface a soft message and
        // keep trying until the overall deadline.
        setError(
          err instanceof Error ? err.message : "Temporary error while checking status.",
        );
        scheduleNext();
      }
    };

    void poll();
    return () => {
      doneRef.current = true;
      if (timer) {
        clearTimeout(timer);
      }
    };
  }, [id, pollIntervalMs, pollTimeoutMs, navigate]);

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
        {phase === "timeout" && (
          <>
            <h1 style={styles.h1}>This is taking longer than expected</h1>
            <p style={styles.subtitle}>
              Your payment went through, but provisioning hasn&apos;t finished
              yet. It may still complete on its own — try refreshing in a few
              minutes. If the problem persists, please contact support and
              we&apos;ll finish setting up your workspace.
            </p>
            <a href="mailto:support@kmail.kchat.dev" style={styles.linkButton}>
              Contact support
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
    padding: "0.8rem 1.4rem",
    borderRadius: 8,
    border: "none",
    background: "#2563eb",
    color: "#fff",
    fontSize: "1rem",
    fontWeight: 600,
    cursor: "pointer",
  },
  back: {
    padding: "0.8rem 1.4rem",
    borderRadius: 8,
    border: "1px solid #cbd5e1",
    background: "#fff",
    color: "#1e293b",
    fontSize: "1rem",
    fontWeight: 600,
    cursor: "pointer",
  },
  navRow: {
    marginTop: "1.5rem",
    display: "flex",
    justifyContent: "space-between",
    alignItems: "center",
    gap: "0.75rem",
  },
  help: {
    margin: "0.5rem 0 0",
    fontSize: "0.85rem",
    color: "#64748b",
    lineHeight: 1.5,
  },
  optional: { fontWeight: 400, color: "#94a3b8" },
  stepper: {
    display: "flex",
    listStyle: "none",
    padding: 0,
    margin: "0 0 1.75rem",
    gap: "0.5rem",
  },
  step: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    flex: 1,
    fontSize: "0.8rem",
    color: "#64748b",
    minWidth: 0,
  },
  stepDot: {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: 26,
    height: 26,
    borderRadius: "50%",
    background: "#e2e8f0",
    color: "#475569",
    fontWeight: 700,
    fontSize: "0.8rem",
    flexShrink: 0,
  },
  stepDotCurrent: { background: "#2563eb", color: "#fff" },
  stepDotDone: { background: "#16a34a", color: "#fff" },
  stepLabel: { whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" },
  review: { margin: "0.5rem 0 0", display: "grid", gap: "0.5rem" },
  reviewRow: {
    display: "flex",
    flexDirection: "column",
    gap: "0.15rem",
    padding: "0.6rem 0",
    borderBottom: "1px solid #e2e8f0",
  },
  reviewLabel: { fontSize: "0.8rem", color: "#64748b", fontWeight: 600 },
  reviewValue: {
    margin: 0,
    display: "flex",
    justifyContent: "space-between",
    alignItems: "center",
    gap: "0.5rem",
    color: "#0f172a",
    fontSize: "1rem",
  },
  editLink: {
    border: "none",
    background: "none",
    color: "#2563eb",
    cursor: "pointer",
    fontSize: "0.85rem",
    fontWeight: 600,
    padding: 0,
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
