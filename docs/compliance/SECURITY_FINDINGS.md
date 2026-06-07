# Security Findings & Remediation Tracker

This is the living triage log for the automated dependency/vuln
scanners wired in [`.github/workflows/security-scan.yml`](../../.github/workflows/security-scan.yml)
and for the manual audit performed during the Session 6 security
hardening pass. The Security Scan workflow is **informational and
non-blocking** by design (separate workflow, not part of the
required `CI Status` aggregate, and every scan job is
`continue-on-error: true`). Promote an individual scanner to a
required check only after its findings here are driven to zero.

Last manual sweep: 2026-06-06 (Session 6 — Security Hardening & SOC 2 prep).

## 1. Code-level audit findings (Step 1) — FIXED

| # | Area | Finding | Resolution |
|---|------|---------|------------|
| F-1 | RLS | 61 of 63 tenant-scoped tables had `ENABLE` but not `FORCE ROW LEVEL SECURITY`; a single-role / table-owner connection could bypass policies if the tenant GUC were ever unset. | `migrations/008_force_rls.sql` — idempotent `DO` block forces RLS on every enabled-but-unforced table. Verified: 0 enabled-not-forced post-migration. Regression test: `TestRLS_ForcedOnEveryEnabledTable`. |
| F-2 | Audit log | `Log()` selected the chain tail with `ORDER BY created_at DESC, id DESC`. `created_at` is `now()` (transaction-start time, not monotonic with commit order under the per-tenant advisory lock) and `id` is a random UUID, so a concurrent append could attach to a non-tail row and **fork the chain**. | (a) Per-tenant `pg_advisory_xact_lock` serialises appends; (b) `migrations/011_audit_seq.sql` adds a monotonic `seq` and both `Log()` and `VerifyChain()` now order by it; (c) `migrations/009_audit_chain_linearity.sql` adds a unique `(tenant_id, prev_hash)` index as a structural backstop. Regression test: `TestAuditChain_ConcurrentWritesStayLinear`. |
| F-3 | TOTP 2FA | `/api/v1/auth/totp/{verify,check}` had no per-account brute-force ceiling; 6-digit codes with a ±1 step window (~3e-6 per guess) are bypassable over a few hours at modest request rates. | `migrations/010_totp_lockout.sql` + `TOTPStore.EvaluateAttempt`: the lock-check, code/recovery verification, and counter/lock update run inside **one `SELECT … FOR UPDATE` transaction**, fully serialising concurrent attempts per account (no check-then-act race). 5 consecutive failures park the account for 15 min; thresholds env-tunable (`KMAIL_TOTP_MAX_FAILED_ATTEMPTS` / `KMAIL_TOTP_LOCKOUT_DURATION`). Regression tests: `TestTOTPLockout_EnforcedPerAccount`, `..._ConcurrentBurstRespectsCeiling`, `..._RecoveryCodeConsumeAtomic`. |
| F-4 | Migrations | Duplicate migration version 6 (`006_confidential_send_mls.sql` and `006_feature_flags.sql`) broke the migration runner's uniqueness check. | Renamed the later file to `007_confidential_send_mls.sql`. |
| F-5 | TOTP 2FA | The `enroll` path resets `failed_attempts`/`locked_until`, so a caller holding only the first factor (OIDC) could re-enroll a fresh secret to clear a standing lockout or replace an active credential without proving possession of the current second factor — a re-enrollment lockout bypass. | `enroll` now routes an **already-enabled** credential through the same `EvaluateAttempt` `FOR UPDATE` lockout path: rotation requires a live TOTP code or an unused recovery code (the lost-authenticator escape hatch), and is refused with 429 while the account is locked. The new (disabled) secret is written atomically and the old recovery bundle cleared; the user re-confirms via `/verify`. First-time or unconfirmed (disabled) enrollment stays frictionless. Regression tests: `TestTOTPEnroll_ReenrollEnabledRequiresFactor`, `..._ReenrollWithValidTOTP`, `..._ReenrollWithRecoveryCode`, `..._ReenrollWhileLockedRefused`, `..._FirstEnrollmentFrictionless`. |

Items reviewed and found **already sound** (no change required):
`internal/middleware/auth.go` / `oidc.go` (OIDC fail-closed; no
authenticate-on-error path), `internal/cmk/envelope.go` (AES-256-GCM
envelope; the only non-GCM path is an explicitly-documented legacy
unwrap, never a write fallback), and
`deploy/helm/kmail/templates/stalwart-mtls.yaml` (cert-manager
issued client cert, `tls.Config` verification airtight in
`internal/jmap/proxy.go`).

## 2. Dependency-scan findings

### 2.1 Go — `govulncheck ./...`

All findings are **Go standard-library** advisories tied to the
build toolchain patch level; none are in first-party code or
third-party modules' *called* surface. Representative items:
`GO-2025-4007` (crypto/x509 name-constraint quadratic blowup, fixed
in go1.25.3), `GO-2025-4006` (net/mail ParseAddress CPU, fixed in
go1.25.2), plus several net/http and net/textproto items.

**Remediation (correct long-term fix): bump the build toolchain to
the latest go1.25 patch release.** CI already uses
`go-version: "1.25.x"`, which resolves to the newest patch at run
time, so CI builds pick up the fixes automatically; the advisories
surface locally only when an older 1.25.0 toolchain is installed.
Keeping the toolchain current (and letting Renovate's `gomod`
updates raise the `go` directive) closes these out. No application
code change is warranted — rewriting around stdlib functions would
be a band-aid.

### 2.2 Web — `npm audit` (`web/`)

| Package | Severity | Shipped to prod? | Disposition |
|---------|----------|------------------|-------------|
| `vitest`, `@vitest/ui`, `vite-node`, `@vitest/mocker` | critical/moderate | No (test/dev tooling) | Upgrade tracked via Renovate; not in the production bundle, so not a runtime exposure. Major bump (vitest 4.x) staged separately to avoid destabilising the 182-test suite. |
| `esbuild` (via `vite`) | moderate | No (dev server only) | Advisory is the dev-server request bug; not reachable in production. Resolved by the same `vite`/`vitest` major bump. |
| `react-router` / `react-router-dom` | moderate | Yes | Open-redirect via protocol-relative `//` path. Non-major fix available (`npm audit fix`); scheduled — low exposure because redirects in-app are server-anchored, but worth patching. |
| `ws` | moderate | Yes | Uninitialized-memory disclosure. Non-major fix available; scheduled. |

Approach: the runtime-affecting, non-breaking fixes
(`react-router`, `ws`) are queued via Renovate grouped updates;
the dev-only criticals are not a production risk and are bundled
into a deliberate major-version upgrade PR so the test/build tooling
migration is reviewed on its own.

### 2.3 Rust SDK — `cargo audit` (`sdk/`)

**Clean** — 0 vulnerabilities. Two `unmaintained` *warnings*
(`bincode` 1.3.3 `RUSTSEC-2025-0141`, `paste` 1.0.15
`RUSTSEC-2024-0436`). Not vulnerabilities; tracked for eventual
replacement. No action required for SOC 2.

## 3. Accepted observations (no change)

* **CSP `style-src 'unsafe-inline'`** — required by the React
  runtime's inline style attributes; `script-src` remains strict
  (no `unsafe-inline`/`unsafe-eval`), so XSS-to-script execution is
  still blocked. Accepted.
* **CSP `connect-src ... https:`** — permits XHR/fetch to arbitrary
  HTTPS origins. Kept to support tenant-configured integration
  endpoints; not a direct vulnerability. Revisit if the set of
  outbound origins can be enumerated.

## 4. How to reproduce locally

```bash
# Go
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...

# Web
cd web && npm ci && npm audit --omit=dev --audit-level=high

# SDK
cd sdk && cargo install cargo-audit --locked && cargo audit

# Bundle all three as dated SOC 2 evidence
scripts/compliance/generate-evidence.sh --deps
```
