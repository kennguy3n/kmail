# KMail Threat Model

Scope: the KMail BFF API and its trust boundaries (clients →
BFF → PostgreSQL / Stalwart shards / zk-object-fabric / KChat
OIDC / Stripe). Methodology: STRIDE per trust boundary, plus a
focused multi-tenant isolation analysis. This document supports
penetration-test scoping and the SOC 2 CC3 (risk assessment) /
CC6 (logical access) criteria. Companion:
[`API_ENDPOINTS.md`](./API_ENDPOINTS.md) (endpoint + auth
inventory) and [`SECURITY_OVERVIEW.md`](./SECURITY_OVERVIEW.md)
(architecture).

## 1. Assets
| Asset | Sensitivity | Store |
|-------|-------------|-------|
| Mail bodies & attachments | High (content) | zk-object-fabric (AES-256-GCM envelope), per-tenant bucket |
| Mailbox metadata (headers, labels) | High | PostgreSQL (RLS, TDE) |
| Tenant/user identity, OIDC subject mapping | High | PostgreSQL `users` (RLS) |
| TOTP secrets / recovery codes, WebAuthn creds | Critical | PostgreSQL, envelope-wrapped |
| Customer CMK material / HSM credentials | Critical | `internal/cmk`, envelope-wrapped; HSM external |
| Audit log | High (integrity) | PostgreSQL hash chain |
| Session tokens | High | Redis/Valkey session store |
| Billing data | Medium | PostgreSQL + Stripe |

## 2. Trust boundaries & actors
1. **Public internet → BFF.** Untrusted clients (browser app, mail clients, Stripe/KChat webhooks). Outermost defense: TLS, security headers/CSP, CORS allow-list, rate limiting, OIDC.
2. **BFF → PostgreSQL.** Tenant context carried via the `app.tenant_id` GUC; RLS is the isolation control.
3. **BFF → Stalwart shard.** Per-tenant shard URL, mTLS client cert.
4. **BFF → zk-object-fabric.** Per-tenant bucket; cross-tenant access impossible at the gateway.
5. **BFF → KChat OIDC / MLS, Stripe.** External identity & payment trust.

## 3. STRIDE analysis

### Spoofing
- **Forged identity / token replay.** Mitigation: OIDC verification against KChat JWKS, fail-closed on missing/invalid claims (`internal/middleware/auth.go`); short-lived sessions with idle timeout and max-concurrent caps (`internal/middleware/session.go`).
- **Second-factor bypass.** Mitigation: TOTP verify/check enforce a durable per-account brute-force lockout evaluated atomically under `SELECT … FOR UPDATE` (migration 010, `TOTPStore.EvaluateAttempt`); WebAuthn available.
- **Lockout bypass via re-enrollment.** Mitigation: re-enrolling an already-enabled credential (which would otherwise reset the lockout/replace the secret) goes through the same `FOR UPDATE` lockout path and requires proving the current second factor — a live TOTP code or an unused recovery code (lost-authenticator escape hatch); refused with 429 while locked. First-time/unconfirmed enrollment is unaffected.
- **Webhook spoofing (Stripe).** Mitigation: Stripe signature verification (`KMAIL_STRIPE_WEBHOOK_SECRET`) on the public webhook route.

### Tampering
- **Audit-log tampering.** Mitigation: SHA-256 prev-hash chain; `VerifyChain` recomputes end-to-end; per-tenant advisory lock + monotonic `seq` (migration 011) keep the chain linear and ordered under concurrency; unique `(tenant_id, prev_hash)` index (migration 009) structurally rejects forks.
- **Request/response tampering in transit.** Mitigation: TLS 1.2+ externally, mTLS to Stalwart.
- **Cross-tenant write.** Mitigation: RLS `WITH CHECK` rejects inserts/updates claiming another tenant; **FORCE** RLS (migration 008) so even table-owner connections are policed. Regression-tested (`rls_db_test.go`).

### Repudiation
- **"I didn't do that" on privileged actions.** Mitigation: every admin route writes an actor-attributed `audit_log` entry; tamper-evident chain; reverse-access proxy with approval (`internal/adminproxy`, `internal/approval`).

### Information disclosure
- **Cross-tenant data leakage** (primary multi-tenant risk). Mitigation: forced RLS, per-tenant shards/buckets, GUC set inside every transaction. See §4.
- **Secret exposure.** Mitigation: TOTP secrets / CMK creds envelope-wrapped (AES-256-GCM); no plaintext write path; secrets never logged.
- **Verbose errors / header leakage.** Mitigation: restrictive CSP, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, no stack traces to clients.
- **Open redirect** (react-router advisory, §SECURITY_FINDINGS) — scheduled patch.

### Denial of service
- **Auth/JMAP flooding.** Mitigation: `middleware.RateLimiter` (Redis-backed, fail-closed in prod) on auth + JMAP; signup funnel rate-limited.
- **Algorithmic complexity (stdlib ParseAddress / x509).** Mitigation: keep Go toolchain patched (see SECURITY_FINDINGS §2.1).
- **TOTP guess flooding.** Mitigation: per-account lockout (migration 010).

### Elevation of privilege
- **Tenant→tenant or user→admin escalation.** Mitigation: RLS scoping; admin routes behind OIDC + approval workflow; SCIM tokens tenant-scoped with RLS.
- **Owner-role RLS bypass.** Mitigation: FORCE RLS (migration 008) closes the table-owner exemption.

## 4. Multi-tenant isolation (deep dive)
The dominant risk for KMail is one tenant reading or mutating
another's data. Defense in depth:
1. **Identity → tenant binding** at the OIDC layer; the tenant id is never client-supplied for data scoping.
2. **`app.tenant_id` GUC** set inside every DB transaction (`middleware.SetTenantGUC`).
3. **RLS policies** on every tenant-scoped table, now **forced** so they apply to all roles (migration 008).
4. **`WITH CHECK`** on policies blocks cross-tenant writes (verified by `TestRLS_CrossTenantIsolation`).
5. **Concurrency**: `TestRLS_ConcurrentTenantsStayIsolated` exercises interleaved access from two tenants under load.
6. **Data plane**: per-tenant Stalwart shard + zk-object-fabric bucket — isolation does not rely on the app alone.

Residual risk: a missing `SetTenantGUC` in a *new* code path. Forced
RLS converts that from a silent leak into "no rows / policy error",
and the schema-invariant test fails CI if any enabled table is left
unforced.

## 5. Out of scope / inherited
- Physical / cloud-provider controls (inherited SOC 2).
- KChat IdP internal security, Stripe internal security (vendor DPAs).
- Stalwart server internals (tracked via `STALWART_UPGRADE.md`).

## 6. Review cadence
Re-review on any change to auth, RLS policies, the audit chain, or
the trust-boundary set, and at minimum once per SOC 2 observation
window.
