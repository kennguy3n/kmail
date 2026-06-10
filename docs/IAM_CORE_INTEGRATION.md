# iam-core OIDC Integration

KMail can use [iam-core](https://github.com/uneycom/iam-core) as its
OIDC identity provider in place of the legacy KChat OIDC issuer. When
enabled, iam-core owns login, consent, and token issuance; KMail
trusts iam-core access tokens, provisions tenants and mailboxes from
iam-core lifecycle webhooks, and reads users/tenants back through the
iam-core Management API for reconciliation and event enrichment.

The integration is **inert by default**. It activates only when
`KMAIL_IAM_CORE_MGMT_URL` is set, and each sub-feature (M2M client,
webhook receiver, lazy provisioning) is independently gated so an
operator can adopt it incrementally.

- [Architecture](#architecture)
- [Configuration](#configuration)
- [Registering KMail in iam-core](#registering-kmail-in-iam-core)
  - [1. SPA application (browser login)](#1-spa-application-browser-login)
  - [2. M2M application (Management API)](#2-m2m-application-management-api)
  - [3. Custom token claims](#3-custom-token-claims)
  - [4. Webhooks](#4-webhooks)
- [Claims mapping & fallback](#claims-mapping--fallback)
- [Provisioning model](#provisioning-model)
- [Example `.env`](#example-env)
- [Helm](#helm)
- [Operational notes](#operational-notes)

## Architecture

```
                 browser login (Authorization Code + PKCE)
   React SPA  ───────────────────────────────────────────►  iam-core
       │                                                       │
       │  access token (JWT)                                   │
       ▼                                                       │
   KMail BFF (kmail-api)                                        │
       │  - validates JWT via iam-core JWKS (middleware/auth)   │
       │  - maps claims → (tenant_id, kchat_user_id)            │
       │                                                        │
       ├── webhook receiver  ◄───── tenant/user lifecycle ──────┤
       │   POST /api/v1/webhooks/iam-core (HMAC-signed)         │
       │                                                        │
       └── M2M client  ─────── Client Credentials ─────────────►┘
           GET /api/v1/management/{users,tenants}
```

Three couplings, each in `internal/iamcore`:

1. **Token validation** (`internal/middleware/auth.go`) — KMail
   verifies iam-core JWTs against the issuer JWKS exactly as it did
   KChat tokens. `KMAIL_KCHAT_OIDC_ISSUER` / `_AUDIENCE` are pointed
   at iam-core. The middleware adds claim fallbacks (below) so
   iam-core tokens resolve to KMail's `(tenant_id, kchat_user_id)`
   identity without forcing a specific claim layout.
2. **Webhook receiver** (`internal/iamcore/webhooks.go`) — a
   signature-verified handler mounted at
   `POST /api/v1/webhooks/iam-core` that turns `tenant.create`,
   `user.create`, `user.update`, and `user.delete` events into
   `tenant.Service` provisioning calls.
3. **M2M client** (`internal/iamcore/client.go`) — a token-cached
   Client Credentials client for the iam-core Management API
   (`GetUser`, `ListUsers`, `GetTenant`), used to enrich sparse
   webhook payloads and for reconciliation tooling.

## Configuration

All settings are read from `KMAIL_IAM_CORE_*` environment variables
(`internal/config/config.go`). Unlike most KMail settings these have
**no** bare-name fallback — they are new and `KMAIL_`-prefixed only.

| Env var | Required | Description |
| --- | --- | --- |
| `KMAIL_IAM_CORE_MGMT_URL` | yes (to enable) | iam-core base URL. Token endpoint is `<url>/oauth2/token`; management API under `<url>/api/v1/management/`. Empty disables the whole integration. |
| `KMAIL_IAM_CORE_M2M_CLIENT_ID` | for M2M client | Client Credentials `client_id` for KMail's M2M app. When unset, the webhook receiver still runs but provisions from event payloads only (no enrichment). |
| `KMAIL_IAM_CORE_M2M_CLIENT_SECRET` | for M2M client | Matching `client_secret`. Store in a Secret. |
| `KMAIL_IAM_CORE_M2M_AUDIENCE` | for M2M client | Token `audience` for the management API. iam-core scopes M2M clients per tenant; the audience host names KMail's management tenant and is sent as `X-Tenant-ID` on the token request. |
| `KMAIL_IAM_CORE_WEBHOOK_SECRET` | for webhooks | Shared HMAC-SHA256 secret. Empty disables the webhook receiver (it returns 503). |
| `KMAIL_IAM_CORE_LAZY_PROVISION` | no (default `false`) | When `true`, provisions a KMail tenant on first authenticated request if a webhook has not already. |

KMail also needs its OIDC issuer pointed at iam-core:

| Env var | Description |
| --- | --- |
| `KMAIL_KCHAT_OIDC_ISSUER` | iam-core issuer URL (drives JWKS discovery). |
| `KMAIL_KCHAT_OIDC_AUDIENCE` | Expected `aud` of user access tokens (KMail's SPA API audience). |

## Registering KMail in iam-core

### 1. SPA application (browser login)

Register KMail's web client as a **SPA / public** application using
Authorization Code + PKCE:

- **Redirect URIs**: `https://mail.<your-domain>/auth/callback` (and
  `http://localhost:5173/auth/callback` for local dev).
- **Grant types**: `authorization_code`, `refresh_token`.
- **Audience**: an API resource representing KMail's BFF (see step 3);
  this becomes `KMAIL_KCHAT_OIDC_AUDIENCE`.
- **Scopes**: `openid profile email` plus any KMail role scopes.

### 2. M2M application (Management API)

Register a second **machine-to-machine** application for KMail's
backend:

- **Grant type**: `client_credentials`.
- **Audience**: the iam-core Management API
  (`https://<mgmt-tenant>/api/v1/management/`). This value becomes
  `KMAIL_IAM_CORE_M2M_AUDIENCE`; its host is the management tenant
  KMail's client is registered in.
- **Granted scopes / permissions**: read access to users and tenants
  (`read:users`, `read:tenants` or the equivalent in your iam-core
  permission model).
- Copy the `client_id` / `client_secret` into
  `KMAIL_IAM_CORE_M2M_CLIENT_ID` / `KMAIL_IAM_CORE_M2M_CLIENT_SECRET`.

### 3. Custom token claims

KMail keys every control-plane row on a tenant UUID and an external
user id. Configure iam-core **Custom Token Claims** on the SPA app's
access token so they arrive directly:

| KMail field | Preferred claim | Fallback (see below) |
| --- | --- | --- |
| Tenant id | `tenant_id` | `https://kmail.io/tenant_id` |
| User id | `kchat_user_id` | `sub` |

Emitting `tenant_id` and `kchat_user_id` directly is recommended.
If your iam-core tier cannot emit unnamespaced custom claims, emit
the namespaced `https://kmail.io/tenant_id` instead — KMail reads it
as a fallback. The bare `sub` claim is always present, so user id
resolution works even with no custom-claim configuration, but
configuring `kchat_user_id` explicitly is preferred for stability.

### 4. Webhooks

Configure iam-core to deliver tenant/user lifecycle events to:

```
POST https://mail.<your-domain>/api/v1/webhooks/iam-core
```

- **Signing**: HMAC-SHA256 with the shared secret you set as
  `KMAIL_IAM_CORE_WEBHOOK_SECRET`. KMail expects the signature in the
  `X-KMail-Signature` header as `t=<unix>,v1=<hex>`, where the signed
  message is `"<unix>." + raw_request_body`. Deliveries older than
  5 minutes (clock skew tolerance) are rejected as replays.
- **Events**: `tenant.create`, `user.create`, `user.update`,
  `user.delete`. Unknown event types are acknowledged with `200` and
  ignored.
- **Payload shape**:

  ```json
  {
    "id": "evt_123",
    "type": "user.create",
    "created_at": 1717000000,
    "data": {
      "tenant_id": "7e1c…uuid",
      "user_id": "auth0|abc",
      "email": "ada@acme.com",
      "name": "Ada Lovelace",
      "display_name": "Ada"
    }
  }
  ```

  `tenant.create` carries `{ "tenant_id", "name", "slug", "plan" }`.
  Sparse user payloads (missing `email`/`name`) are backfilled from
  the Management API when the M2M client is configured.

KMail responds `200` on success, `400` on a malformed body, `401` on
a bad/missing signature, and `500` when a downstream provisioning
call fails — so iam-core should **retry on 5xx** (at-least-once
delivery is safe: all handlers are idempotent).

## Claims mapping & fallback

`internal/middleware/auth.go` resolves identity from token claims in
this order (logging a warning the first time a fallback is used, so
operators know to configure custom claims):

- **tenant id**: `tenant_id` → else `https://kmail.io/tenant_id`.
- **user id**: `kchat_user_id` → else `sub`.

If neither path yields a value the request is rejected `401`. This
keeps legacy KChat tokens (which carry `tenant_id` / `kchat_user_id`
directly) working unchanged while accepting iam-core tokens.

## Provisioning model

KMail keeps the iam-core tenant id and the KMail tenant UUID **1:1**
via `tenant.Service.EnsureTenant`, which is idempotent and id-explicit
(it inserts with `ON CONFLICT (id) DO NOTHING` and re-reads on a lost
race). Two paths converge on it:

- **Webhook-driven** (preferred): `tenant.create` → `EnsureTenant`;
  `user.create` → `CreateUser`; `user.update` → `UpdateUser` (with
  provision-on-miss); `user.delete` → `DeleteUser`. Every handler is
  safe under redelivery — duplicate creates become no-ops, deletes
  for unknown users are no-ops.

  > **Provision-on-miss needs the M2M client for sparse updates.** A
  > `user.update` for a user KMail hasn't provisioned yet falls
  > through to `CreateUser` so the update isn't dropped. A mailbox
  > requires an email address, so if the update payload is sparse
  > (no `email`) **and** the M2M client is not configured to backfill
  > it, `CreateUser` rejects the input and the delivery `500`s —
  > iam-core then retries until the authoritative `user.create`
  > arrives (carrying the email) or the M2M client is configured.
  > KMail deliberately never fabricates a placeholder address: a
  > wrong mailbox address is worse than a retried webhook. Configure
  > the M2M client for reliable provision-on-miss.
- **Lazy** (`KMAIL_IAM_CORE_LAZY_PROVISION=true`): on the first
  authenticated request for a tenant whose row does not yet exist
  (e.g. a webhook was lost), KMail provisions it inline with a
  placeholder name/slug (derived from the tenant UUID). Results are
  cached in Valkey for 5 minutes so the check stays off the hot path,
  and the middleware **fails open** — a provisioning error logs and
  the request proceeds rather than 500-ing.

When both paths run for the same tenant (a lazy provision followed by
a later `tenant.create` webhook), `EnsureTenant` **reconciles** the
authoritative iam-core `name`/`slug`/`plan` from the webhook onto the
placeholder row rather than leaving the UUID-derived placeholders in
place. Id-only callers (lazy provisioning) never overwrite metadata, so
the webhook remains the source of truth for tenant metadata.

Mailbox account ids are derived deterministically from the iam-core
user id (`iam-<user_id>`) so redelivered `user.create` events resolve
to the same Stalwart account.

`user.create` ensures the tenant row exists (id-only `EnsureTenant`)
before inserting the mailbox, so an out-of-order delivery (a
`user.create` arriving before its `tenant.create`) does not fail the
`users.tenant_id` foreign key and retry-storm. The placeholder it
leaves is reconciled by the authoritative `tenant.create` as above.

**Mailbox addresses are immutable from webhooks.** A user's email is
the mailbox's primary address; renaming it is a side-effectful
Stalwart/alias operation, not a metadata update, so `user.update`
does **not** auto-apply email changes (`UpdateUserInput` exposes no
`Email` field). If iam-core reports an email that differs from KMail's
stored address, the handler logs a warning identifying the user and
both addresses so operators can reconcile the rename deliberately; the
`display_name` metadata is still updated.

### Development without a real issuer

When `KMAIL_ENV=development` and no OIDC issuer/JWKS is configured,
KMail accepts an unverified bearer JWT so contributors can hit
endpoints without a live issuer. This dev path applies the **same
iam-core claim fallbacks** as production (the namespaced
`https://kmail.io/tenant_id` claim and the standard `sub`), so an
iam-core-style token is accepted locally exactly as it would be once
verified in production. This path is closed outside development.

## Example `.env`

```bash
# --- Point KMail's OIDC validation at iam-core ---
KMAIL_KCHAT_OIDC_ISSUER=https://auth.kmail.io
KMAIL_KCHAT_OIDC_AUDIENCE=https://api.kmail.io

# --- iam-core Management API (M2M) ---
KMAIL_IAM_CORE_MGMT_URL=https://auth.kmail.io
KMAIL_IAM_CORE_M2M_CLIENT_ID=kmail-backend
KMAIL_IAM_CORE_M2M_CLIENT_SECRET=change-me
KMAIL_IAM_CORE_M2M_AUDIENCE=https://kmail-mgmt.auth.kmail.io/api/v1/management/

# --- Webhooks ---
KMAIL_IAM_CORE_WEBHOOK_SECRET=change-me-too

# --- Optional: provision tenants lazily if a webhook is missed ---
KMAIL_IAM_CORE_LAZY_PROVISION=true
```

## Helm

`deploy/helm/kmail/values.yaml` exposes an `iamCore` block:

```yaml
iamCore:
  enabled: false
  mgmtURL: ""
  m2mClientID: ""
  m2mAudience: ""
  lazyProvision: false
  # Names of existing Kubernetes Secrets holding the sensitive
  # values. Each Secret must contain the value under a key equal to
  # the env var name (e.g. KMAIL_IAM_CORE_M2M_CLIENT_SECRET). Leave
  # empty to omit that env var (e.g. disable the webhook receiver).
  m2mClientSecretRef: ""
  webhookSecretRef: ""
```

When `iamCore.enabled` is true, `deployment-api.yaml` injects the
`KMAIL_IAM_CORE_*` env vars — non-secret values inline, and the M2M
client secret / webhook secret via `secretKeyRef` to the referenced
Secrets.

## Operational notes

- **Disabling the internal OAuth2 server**: when iam-core is enabled,
  KMail does not mount its own authorization-server endpoints
  (`/api/v1/oauth/authorize|token|revoke`) — iam-core is the
  authority. KMail's TOTP step-up endpoints (Confidential Send, Vault
  unlock) stay active. Previously-issued tokens for installed
  third-party integrations are still validated by the integration
  gateway.
- **Token caching**: the M2M client caches the access token and
  refreshes 30s before expiry under a mutex, so concurrent callers
  share a single in-flight refresh.
- **Fail-closed vs fail-open**: signature verification and token
  validation fail **closed** (reject). Lazy provisioning fails
  **open** (log + proceed) because it is a convenience for webhook
  gaps, not an authorization decision.
- **RLS**: user provisioning runs inside the tenant-scoped GUC so
  Row-Level Security validates every write; `tenants` itself is a
  control-plane table not under RLS.
