# API Endpoint & Authentication Inventory

Inventory of the KMail BFF HTTP surface and the authentication
mechanism guarding each route, for penetration-test scoping and
SOC 2 CC6 (logical access) evidence. Routes are registered in
[`cmd/kmail-api/main.go`](../../cmd/kmail-api/main.go) and the
per-subsystem `internal/*/handlers.go` `Register(...)` functions.

> Regenerate the raw route list (sorted, unique) with:
> ```bash
> grep -rhoE '"(GET|POST|PUT|PATCH|DELETE) /[^"]+"' internal/ cmd/ | sort -u
> ```
> Auth is determined by the wrapper passed to each `Register(mux, <wrapper>)`
> call in `main.go`; see the legend below.

## Middleware chain (outermost → innermost)
Every response passes through, in order:
1. **Security headers + CORS** (`middleware.Security`) — CSP, HSTS, `X-Frame-Options: DENY`, `nosniff`, CORS allow-list from `KMAIL_CORS_ORIGINS`.
2. **RequestID** → **RequestLogger** → **Metrics** → **Tracing** (observability).
3. Per-route **auth wrapper** (below). Public routes skip this layer.
4. **Rate limiter** (`middleware.RateLimiter`, Redis/Valkey, fail-closed in prod) — inserted *inside* auth where applied, so it can read tenant/user identity.
5. The subsystem handler (service-layer input validation).

## Auth-wrapper legend
| Wrapper (in `main.go`) | Mechanism | Used for |
|------------------------|-----------|----------|
| `authMW` / `authMW.Wrap` | **OIDC** JWT verified against KChat JWKS, **fail-closed** on missing/invalid claims (`internal/middleware/auth.go`, `oidc.go`). Sets tenant/user in context. | The large majority of `/api/v1/**` routes. |
| `wrapAuthRL` | OIDC + **session enforcement** + **rate limiter**. | `/jmap`, `/jmap/` (mail data plane). |
| `wrapSessionAPI` | OIDC + rate limiter. | `/api/v1/sessions*` (session management). |
| `oauthAuthMW` | OAuth access-token middleware (`oauth.NewAuthMiddleware`). | `/api/v1/integ*` (third-party integrations). |
| `oauthAuthMW`/`authMW` (OAuth provider) | OIDC for the management UI; OAuth flow endpoints under `/api/v1/oauth`. | `/api/v1/oauth/*`. |
| **(public)** | **No auth wrapper.** Protected by other means (signature, network policy, single-use token, or intentionally public). | See §Public below. |

## Public / unauthenticated endpoints (pentest-critical)
These are registered with `Register(mux)` (no auth wrapper) or
explicitly mounted public. They are the primary external attack
surface and warrant the most pentest attention.

| Method/Path | Purpose | Compensating control |
|-------------|---------|----------------------|
| `GET /healthz` | Liveness probe | None needed; no data. |
| `GET /readyz` | Readiness (DB ping) | No tenant data returned. |
| `GET /metrics` | Prometheus metrics | Should be network-restricted (scrape-only); not behind OIDC by design. |
| `POST /api/v1/stripe/webhook` (Stripe webhook) | Billing lifecycle events | **Stripe signature verification** (`KMAIL_STRIPE_WEBHOOK_SECRET`). |
| `POST /api/v1/signup` | Self-serve tenant signup funnel | **Rate-limited** (`limiterStore`), trusted-proxy depth bounded; mints Stripe checkout. |
| `GET /api/v1/secure/{token}` | Confidential-Send public recipient portal | **Single-use/scoped token**; optional password; MLS-wrapped DEK. Admin routes for the same feature stay behind `authMW`. |
| `GET /.well-known/autoconfig/mail/config-v1.1.xml`, `GET /mail/config-v1.1.xml` | Thunderbird autoconfig | Public by protocol; emits server hostnames only. |
| `GET /autodiscover/autodiscover.xml`, `GET /Autodiscover/Autodiscover.xml` | Outlook autodiscover | Public by protocol; non-sensitive. |
| `GET /.well-known/caldav` | CalDAV discovery | Public by protocol. |

> Verify periodically that **no other** route is registered without
> a wrapper. In `main.go`, public registrations are the
> `Register(mux)` calls (no second arg); everything else passes
> `authMW`, `wrapAuthRL`, `wrapSessionAPI`, or `oauthAuthMW`.

## Authenticated endpoint groups
All routes below require **OIDC** (`authMW`) unless noted; the
table groups by prefix. Counts are approximate (sub-resources
nest under each prefix).

| Prefix | Auth | Handler | Notes |
|--------|------|---------|-------|
| `/jmap`, `/jmap/` | OIDC + session + rate-limit (`wrapAuthRL`) | `internal/jmap` (proxy) | Mail data plane to per-tenant Stalwart shard over mTLS. |
| `/api/v1/tenants/**` | OIDC | `internal/tenant` | Largest group; tenant/user/mailbox config, calendars, contacts nested under tenant scope. |
| `/api/v1/admin/**` | OIDC (+ approval) | `internal/adminproxy`, `internal/approval`, `internal/featureflags` | Privileged ops; reverse-access proxy + approval workflow; all writes audit-logged. |
| `/api/v1/auth/**` | OIDC | `internal/middleware` (TOTP, WebAuthn) | TOTP `verify`/`check` per-account lockout-protected; `enroll` **and** `DELETE /api/v1/auth/totp` (`disable`) require the current second factor (TOTP/recovery code) to rotate or remove an already-enabled credential, through the same lockout path. |
| `/api/v1/sessions*` | OIDC + rate-limit (`wrapSessionAPI`) | `internal/middleware/session_handlers.go` | Session list/revoke. |
| `/api/v1/calendars/**`, `/api/v1/resource-calendars/**` | OIDC | `internal/*` (calendar) | Includes `/freebusy`. |
| `/api/v1/contacts/**` | OIDC | contact bridge | CardDAV-backed. |
| `/api/v1/emails`, `/api/v1/send`, `/api/v1/scheduled-sends`, `/api/v1/snooze(d)`, `/api/v1/undo*` | OIDC | send/scheduledsend/snooze/undosend | |
| `/api/v1/attachments/**`, `/api/v1/storage/**` | OIDC | `internal/jmap/attachment_handlers.go` | zk-object-fabric backed. |
| `/api/v1/search`, `/api/v1/priority-inbox`, `/api/v1/email-analytics`, `/api/v1/smart*` | OIDC | search/priority/smartfeatures | |
| `/api/v1/shared-inboxes/**` | OIDC | `internal/sharedinbox` | |
| `/api/v1/migrations/**` | OIDC | `internal/migration` | Mailbox import/cutover. |
| `/api/v1/push/**` | OIDC | `internal/push` | FCM/web push registration. |
| `/api/v1/integ*` | OAuth token (`oauthAuthMW`) | `internal/integrations` | Third-party integrations. |
| `/api/v1/oauth/*` | OIDC / OAuth flow | `internal/oauth` | OAuth provider endpoints. |
| `/api/v1/chat-bridge/**`, `/api/v1/sync` | OIDC | chat bridge / sync | |
| `/api/v1/billing/**` | OIDC | `internal/billing` | Stripe-backed (webhook is the only public billing route). |
| `/api/v1/malware`, `/api/v1/audit/**`, `/api/v1/cmk/**`, `/api/v1/vault/**`, `/api/v1/retention/**`, `/api/v1/onboarding/**`, `/api/v1/export/**` | OIDC | respective packages | CMK/vault routes refuse if `KMAIL_SECRETS_KEY` unset. |
| `/scim/v2/**` (Users, Groups, Schemas, ResourceTypes, ServiceProviderConfig) | **SCIM bearer token** (tenant-scoped, RLS) | `internal/scim` | Provisioning; tokens in `scim_tokens` with RLS. |

## Notes for pentesters
- **Tenant isolation** is the highest-value target: attempt cross-tenant reads/writes on every `/api/v1/tenants/**` and `/jmap` route. Forced RLS (migration 008) + `WITH CHECK` should make all such attempts return no rows or a policy error.
- **TOTP** `verify`/`check`: confirm the per-account lockout (5 failures → 15-min 429) cannot be reset by rotating IPs or parallel requests (lockout is keyed on `(tenant_id, user_id)`, incremented atomically).
- **TOTP** `enroll`: confirm it cannot be used to bypass the lockout — re-enrolling an already-enabled credential must require a valid current TOTP/recovery code and must be refused with 429 while the account is locked (first-time/unconfirmed enrollment is intentionally frictionless).
- **TOTP** `disable` (`DELETE /api/v1/auth/totp`): confirm the delete-then-re-enroll bypass is closed — removing an already-enabled credential must require a valid current TOTP/recovery code (sent in the body) and must be refused with 429 while the account is locked. Removing a not-yet-confirmed credential, or when nothing is enrolled, is frictionless (204).
- **Confidential-Send portal** (`/api/v1/secure/{token}`): test token guessing, reuse after consumption, and password brute-force.
- **Signup** and **Stripe webhook**: test rate-limit evasion and webhook signature bypass respectively.
