# KMail — JMAP Client API Contract

**License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).

> Status: Phase 1 — Foundation. This document is a contract
> specification, not an implementation. It pins the JMAP surface the
> Go BFF exposes to the React client and the semantics the BFF
> guarantees in front of Stalwart. See
> [ARCHITECTURE.md §7](ARCHITECTURE.md) for the Go service topology
> and [PROPOSAL.md §5](PROPOSAL.md) for service ownership.

---

## 1. Scope and Non-Goals

### 1.1 Scope

- Defines which JMAP capabilities the Go BFF proxies to Stalwart.
- Defines how KChat OIDC auth is translated into Stalwart auth on
  every BFF → Stalwart request.
- Defines capability negotiation between the React client and the
  BFF.
- Defines JMAP push semantics (EventSource vs WebSocket), push
  subscription lifecycle, and how push events fan into KChat
  notifications.
- Defines request batching and error handling conventions at the
  BFF layer.
- Defines rate limiting at the BFF layer.

### 1.2 Non-goals

- Does not redefine the JMAP core spec. The authoritative JMAP
  specs are
  [RFC 8620 (Core)](https://www.rfc-editor.org/rfc/rfc8620) and
  [RFC 8621 (Mail)](https://www.rfc-editor.org/rfc/rfc8621) plus the
  IANA JMAP capability registry.
- Does not define IMAP / SMTP / CalDAV / CardDAV contracts. Those
  are handled directly by Stalwart for third-party clients
  (Thunderbird, Apple Mail) and are out of scope for the BFF.
- Does not replace Stalwart's JMAP implementation. The BFF is a
  thin, tenant-policy-enforcing proxy.

---

## 2. Capability Surface

### 2.1 JMAP capabilities proxied to Stalwart

The BFF proxies the following JMAP capabilities to Stalwart. Every
capability in this table has a corresponding entry in
[IANA's JMAP capability registry](https://www.iana.org/assignments/jmap/jmap.xhtml).

| Capability URI                                 | Source     | Client exposure | Notes                                                                              |
| ---------------------------------------------- | ---------- | --------------- | ---------------------------------------------------------------------------------- |
| `urn:ietf:params:jmap:core`                    | RFC 8620   | Always          | Required. Session, batch, push, upload/download primitives.                        |
| `urn:ietf:params:jmap:mail`                    | RFC 8621   | Always          | `Mailbox`, `Email`, `Thread`, `EmailSubmission` (query only), `SearchSnippet`.     |
| `urn:ietf:params:jmap:submission`              | RFC 8621   | Plan-gated      | Outbound send. Gated by tenant plan, rate limit, and Confidential-Send policy.    |
| `urn:ietf:params:jmap:vacationresponse`        | RFC 8621   | Always          | Tenant admins may disable per-user if desired.                                     |
| `urn:ietf:params:jmap:calendars`               | Draft      | Always          | Personal, team, and resource calendars. Backed by Stalwart's CalDAV store.         |
| `urn:ietf:params:jmap:contacts`                | Draft      | Always          | Backed by Stalwart's CardDAV store.                                                |
| `urn:ietf:params:jmap:websocket`               | RFC 8887   | Always          | WebSocket push. Preferred transport for browser push; see §5.                      |
| `urn:ietf:params:jmap:sieve`                   | Draft      | Plan-gated      | Pro / Privacy tiers. Admins can disable per-tenant.                                |
| `urn:ietf:params:jmap:quota`                   | RFC 9425   | Always          | Read-only view; quota accounting is authoritative in the Billing service.          |
| `urn:ietf:params:jmap:blob`                    | RFC 9404   | Always          | Attachment upload / download. Backed by zk-object-fabric presigned URLs (§5).      |

### 2.2 KMail extension capabilities

The BFF advertises a small number of KMail-specific extensions,
namespaced under `https://kmail.kchat.example/jmap/`:

| Capability URI                                             | Purpose                                                                                     |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| `https://kmail.kchat.example/jmap/confidential-send`       | Marks a `Email/set create` as Confidential Send (MLS-derived envelope, StrictZK blob).     |
| `https://kmail.kchat.example/jmap/shared-inbox`            | Surfaces shared-inbox ACLs on `Mailbox` objects (read-only; mutated via Tenant Service).    |
| `https://kmail.kchat.example/jmap/chat-bridge`             | Attaches KChat thread references to `Email` objects for share-to-channel flows.            |
| `https://kmail.kchat.example/jmap/vault`                   | Marks mailboxes as Zero-Access Vault; search is disabled on these mailboxes (§2.4).        |

All KMail extensions are **optional**. Clients that do not advertise
support receive the base JMAP behavior. Extensions never change the
meaning of base RFC objects — they add side-channel metadata only.

### 2.3 Capabilities NOT exposed

The following Stalwart-side capabilities are deliberately stripped
at the BFF before returning to the client:

- **`urn:ietf:params:jmap:core:push` streaming endpoints that bypass
  the BFF.** Clients must subscribe via the BFF so that push events
  are authorized, rate-limited, and rewritten for KChat integration.
- **Administrative capabilities.** Stalwart's admin / provisioning
  JMAP is called by the Tenant Service and DNS Onboarding service
  directly and is not exposed to end-user clients.
- **Any draft capability not in the table in §2.1.** Unknown
  capabilities from Stalwart upgrades are hidden from the client by
  default (fail-closed) until the BFF explicitly adds them to the
  allowlist.

### 2.4 Vault mailboxes

Mailboxes marked with the `.../jmap/vault` extension are
Zero-Access Vault folders. The BFF enforces:

- `Email/query` with full-text search filters is rejected
  (`cannotSearch` error) on vault mailboxes.
- `SearchSnippet/get` returns no snippet data for messages in vault
  mailboxes.
- `Email/get` returns the ciphertext blob reference; the React
  client decrypts locally using MLS-derived folder keys.

---

## 3. Authentication and Authorization

### 3.1 Client → BFF authentication

- Clients authenticate to the BFF using the same KChat OIDC token
  they use for chat. No separate KMail credential exists.
- Token transport: HTTPS only. The BFF requires `Authorization:
  Bearer <token>` on every JMAP request.
- Token validation: the BFF validates the OIDC token against the
  KChat identity provider's JWKS, with a 5-minute clock skew and
  120-second audience check. Tokens are cached per `kid` in
  Valkey with a TTL equal to the JWKS cache-control horizon.
- On token expiry the BFF returns HTTP 401 with a
  `WWW-Authenticate: Bearer error="invalid_token"` header and an
  application-level JMAP `urn:ietf:params:jmap:error:unauthorized`
  error in the response body for in-session discovery.

### 3.2 BFF → Stalwart authentication

- Stalwart is configured with OIDC as an authentication source and
  accepts bearer tokens issued by the BFF's internal OIDC issuer,
  not the client-facing KChat issuer.
- Per request, the BFF mints a short-lived (60 s) internal service
  token that carries:
  - `sub`: the Stalwart account identifier for the authenticated
    user (resolved from the tenant service — see §3.3).
  - `tid`: the tenant identifier.
  - `scope`: a pinned scope string that restricts the token to JMAP
    data-plane operations (no admin operations).
  - `act`: the acting KChat user ID (for audit correlation).
- The token is signed by a BFF-owned key that Stalwart trusts via
  JWKS discovery on the BFF's internal service endpoint.
- **No password or long-lived credential is ever forwarded to
  Stalwart.** This is what lets us rotate the BFF's signing key
  without user-visible re-auth and keeps client-side credentials
  out of Stalwart's log surface.

### 3.3 KChat identity → Stalwart account resolution

- The Tenant Service holds the mapping `(tenant_id, kchat_user_id)
  → stalwart_account_id`.
- The BFF resolves this mapping on every request from a small LRU
  cache (10 000 entries, 5-minute TTL). Cache misses fetch from the
  Tenant Service via the shared Postgres pool.
- Account provisioning (create, suspend, rotate, delete) is owned
  by the Tenant Service. The BFF never creates Stalwart accounts
  on the fly.

### 3.4 Shared-inbox and delegated access

- Shared inboxes (`sales@`, `support@`) are MLS-group-backed in
  KChat. The BFF advertises the shared inbox as a `Mailbox` on
  every current member's session, but only if the user's KChat MLS
  membership is current for that group.
- Delegated access (e.g., an EA seeing a principal's inbox) is
  modeled as an ACL on the `Mailbox` object and is authoritative in
  the Tenant Service. The BFF re-checks ACLs on every request —
  stale cached ACLs must never leak access.

---

## 4. Capability Negotiation

### 4.1 Session object

The React client issues `GET /.well-known/jmap` (spec-mandated
redirect) which routes to the BFF and returns the JMAP session
object. The BFF constructs this object per-user:

- `capabilities`: the intersection of (BFF allowlist from §2.1) ∩
  (Stalwart advertised capabilities) ∩ (tenant plan gating).
- `accounts`: the Stalwart accounts the authenticated user can see
  (their own, plus any shared inboxes they are currently entitled
  to).
- `primaryAccounts`: the user's personal mailbox account ID per
  capability.
- `apiUrl`, `downloadUrl`, `uploadUrl`, `eventSourceUrl`: all point
  at BFF endpoints, never at Stalwart directly. This is what keeps
  every data-plane request under BFF policy.

### 4.2 Plan gating

Capabilities gated by plan (see [PROPOSAL.md §11](PROPOSAL.md#11-cost-model))
are removed from `capabilities` when the tenant plan does not
include them:

| Plan                  | Confidential Send | Sieve | Vault | Shared inbox (unpaid members) |
| --------------------- | ----------------- | ----- | ----- | ----------------------------- |
| KChat Core Email ($3) | —                 | —     | —     | yes (MLS group members only)  |
| KChat Mail Pro ($6)   | yes               | yes   | —     | yes                           |
| KChat Privacy ($9)    | yes               | yes   | yes   | yes                           |

Gating is advertised, not enforced solely at the client: every
`Email/set` with a Confidential Send flag is re-checked server-side
before relaying to Stalwart, and plan changes invalidate the
session's cached capability set within one Valkey TTL (60 s).

### 4.3 Version negotiation

- The BFF pins each JMAP capability URI to a specific Stalwart
  version. A Stalwart upgrade that changes a capability's observable
  shape requires a BFF release that acknowledges the new version;
  until then the capability is withheld.
- Deprecated capabilities are advertised with a response header
  `Sunset: <rfc7231-date>` on the session endpoint so clients can
  migrate ahead of removal.

---

## 5. Push Semantics

### 5.1 Transport choice

KMail supports both JMAP push transports from RFC 8620 / RFC 8887:

- **WebSocket (`urn:ietf:params:jmap:websocket`)** — preferred for
  browser clients and React Native. One bidirectional connection per
  session. Multiplexed over HTTP/2 where available, falls back to
  HTTP/1.1 with `Upgrade: websocket`.
- **EventSource (SSE)** — fallback for constrained environments
  (enterprise proxies that terminate WebSocket). Unidirectional,
  one connection per session.

The BFF advertises both endpoints in the session object and the
React client prefers WebSocket. Push subscriptions persist through
transport renegotiation.

### 5.2 Subscription lifecycle

- `PushSubscription/set` creates a server-side subscription keyed
  on `(kchat_user_id, device_id, capability_filter)`.
- Subscriptions auto-expire after 7 days of inactivity and are
  refreshed opportunistically on each active JMAP request
  (`updated` verification token round-trip).
- On logout, the BFF deletes all push subscriptions for the
  `(kchat_user_id, device_id)` tuple.
- The BFF holds subscription state in Postgres (authoritative) with
  a Valkey cache for hot reads. Stalwart's view is authoritative
  for the underlying mailbox change set; the BFF owns the
  subscription-to-user mapping and the KChat-notification fan-out.

### 5.3 Change fan-out

On a mailbox change event from Stalwart the BFF:

1. Receives the JMAP `StateChange` via its upstream subscription to
   Stalwart's admin push channel.
2. Resolves the change to `(tenant_id, account_id, mailbox_id)` and
   looks up all active subscriptions.
3. Filters out subscriptions that do not opt into the affected
   capability (e.g., a calendar-only subscription ignores `Email`
   changes).
4. Writes the `StateChange` to each active WebSocket / EventSource
   connection.
5. **Emits a KChat notification** through the Chat Bridge for
   subscriptions that opt into push notifications, subject to the
   user's per-device notification preferences. For Vault mailboxes
   the notification payload is header-only (no preview) because
   the BFF has no plaintext.

### 5.4 Backpressure

- WebSocket writes use bounded per-connection buffers (16 KB). If a
  client is slow the connection is closed with code 1013 (`try
  again later`); the client reconnects and re-syncs via
  `Mailbox/changes` and `Email/changes`.
- EventSource connections that back up are closed with HTTP 204; the
  client reconnects.
- No push event is allowed to block the BFF's main JMAP data-plane
  request loop.

---

## 6. Request Batching

### 6.1 Core/batch semantics

- The BFF accepts JMAP `Invocation[]` batches on the API URL per
  RFC 8620 §3.6.
- Maximum 16 invocations per batch. Oversized batches return
  `urn:ietf:params:jmap:error:limit` with `maxSize=16`.
- Result references (`#ref`) are resolved by Stalwart; the BFF does
  not re-implement result-reference resolution.

### 6.2 Tenant scoping

Every invocation is implicitly scoped to the authenticated user's
tenant. Any attempt to cite an `accountId` that does not belong to
the authenticated user's tenant returns
`urn:ietf:params:jmap:error:accountNotFound`.

### 6.3 Long-running operations

- `Email/import`, `Blob/upload`, and large `Mailbox/query` calls
  may exceed the default 60 s BFF timeout. The BFF issues an
  asynchronous operation ticket (via `async-op` response header)
  that the client polls via `CoreEcho` for completion state.
- The BFF does not open-endedly hold sockets for Stalwart calls
  that exceed its tail-latency SLO; instead it returns a ticket
  and tracks the upstream call on a separate worker.

---

## 7. Error Handling

### 7.1 Error categories

| BFF-visible error                          | HTTP status | JMAP error type                                 | Cause                                                              |
| ------------------------------------------ | ----------- | ----------------------------------------------- | ------------------------------------------------------------------ |
| Missing / invalid token                    | 401         | `urn:ietf:params:jmap:error:unauthorized`       | Token absent, expired, or signature invalid.                       |
| Tenant mismatch                            | 403         | `urn:ietf:params:jmap:error:forbidden`          | Caller attempted cross-tenant access.                              |
| Plan gating                                | 402         | `urn:ietf:params:jmap:error:forbidden`          | Capability not available on tenant plan.                           |
| Rate limit                                 | 429         | `urn:ietf:params:jmap:error:rateLimit`          | Tenant or user rate limit exceeded (see §8).                       |
| Stalwart 5xx                               | 503         | `urn:ietf:params:jmap:error:serverUnavailable`  | Upstream unavailable; retry with backoff.                          |
| Stalwart 4xx that is not a known JMAP code | 500         | `urn:ietf:params:jmap:error:serverFail`         | Unexpected upstream failure; logged with a correlation ID.         |
| Confidential-Send policy violation         | 403         | `urn:ietf:params:jmap:error:forbidden`          | Sender tried Confidential Send on an unsupported plan or recipient.|

### 7.2 Retry guidance

- All 5xx and 429 responses include `Retry-After` with a
  jittered-backoff hint.
- `urn:ietf:params:jmap:error:serverUnavailable` is safe to retry;
  `urn:ietf:params:jmap:error:serverFail` is not (may have had a
  side effect).
- Clients must treat any response without a BFF correlation header
  (`X-KMail-Correlation-Id`) as a network error and retry through
  the fallback transport.

### 7.3 Correlation IDs

Every BFF response carries `X-KMail-Correlation-Id`, propagated
into Stalwart via `traceparent` (W3C Trace Context). Support
tickets reference the correlation ID; logs are indexed on it.

---

## 8. Rate Limiting

Rate limits are enforced at the BFF layer in Valkey (token bucket)
and are tenant-scoped. Stalwart applies a second, coarser limit as
a safety net.

| Bucket                         | Default limit                 | Scope                         | Error response                     |
| ------------------------------ | ----------------------------- | ----------------------------- | ---------------------------------- |
| Session endpoint               | 30 req/min/user               | `(tenant_id, user_id)`        | 429 + `urn:...:rateLimit`          |
| JMAP API endpoint              | 300 req/min/user              | `(tenant_id, user_id)`        | 429 + `urn:...:rateLimit`          |
| `EmailSubmission/set` (send)   | Tenant plan cap (per §9.2)    | `(tenant_id)`                 | 429 + `urn:...:rateLimit`          |
| `Blob/upload`                  | 100 MB/min/user, 1 GB/day/user| `(tenant_id, user_id)`        | 429 + `urn:...:rateLimit`          |
| Push subscription creation     | 10 req/min/user               | `(tenant_id, user_id)`        | 429 + `urn:...:rateLimit`          |
| WebSocket connect              | 5 conn/min/user               | `(tenant_id, user_id)`        | HTTP 429 on upgrade                |

- Tenant-level limits take precedence over user-level limits — a
  single abusive user cannot consume the full tenant budget.
- Deliverability Control Plane can lower the send-rate bucket
  dynamically for a tenant in the restricted IP pool (see
  [PROPOSAL.md §9](PROPOSAL.md)).
- All rate-limit responses include `X-RateLimit-Limit`,
  `X-RateLimit-Remaining`, and `X-RateLimit-Reset` headers.

---

## 9. Observability

- Every request emits a structured log with `tenant_id`, `user_id`,
  `account_id`, `capability`, `method`, `latency_ms`,
  `upstream_status`.
- SLO metrics: P95 session fetch < 100 ms, P95 JMAP batch < 300 ms,
  P99 WebSocket push end-to-end < 500 ms.
- Errors are bucketed by `jmap_error_type` so dashboards can
  distinguish auth, rate, and upstream failures without log
  scraping.

---

## 10. Compatibility Matrix

| Client                    | Transport                  | Auth                       | Push             |
| ------------------------- | -------------------------- | -------------------------- | ---------------- |
| KChat React web           | JMAP over HTTPS            | KChat OIDC bearer          | WebSocket        |
| KChat React Native mobile | JMAP over HTTPS            | KChat OIDC bearer + APNs   | WebSocket + APNs |
| Thunderbird / Apple Mail  | IMAP / SMTP direct to Stalwart | Stalwart OIDC (distinct)| IMAP IDLE        |
| CalDAV clients            | CalDAV direct to Stalwart  | Stalwart OIDC (distinct)   | n/a              |
| Admin console (React)     | Admin API (separate BFF)   | KChat OIDC + admin role    | WebSocket        |

Third-party clients do not go through the BFF; they speak native
IMAP / SMTP / CalDAV directly to Stalwart. The BFF's JMAP contract
applies **only** to KChat first-party clients.
