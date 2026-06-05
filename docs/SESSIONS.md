# Session Management

KMail authenticates with stateless KChat OIDC bearer tokens — the
JWT's `exp` is the authoritative bound on a token's lifetime. On top
of that, KMail keeps a lightweight **server-side session ledger** that
adds three things a bare token cannot:

1. **Concurrent-session cap** — at most _N_ live sessions per user
   (default 5). When a new device pushes past the cap, the oldest
   session is evicted.
2. **Revocation** — a user (or an operator) can revoke a session so
   its token is refused at the KMail boundary *before* the JWT would
   naturally expire.
3. **Visibility** — the "Active sessions" panel in Admin → Security
   lists where the user is currently signed in.

## Model

- A session is keyed by a **salted hash of the bearer token**
  (`SHA-256`, truncated to 32 hex chars). The raw token is never
  stored; the same token always maps to the same session id.
- Sessions are namespaced per `(tenant, user)` so two tenants sharing
  a KChat user id never collide.
- **Idle timeout** (default 8h) is implemented as a TTL on the session
  record: a session untouched for the idle window drops off the
  active list and frees its concurrency slot. It does *not* by itself
  invalidate a still-unexpired JWT — that is what revocation is for.
- The ledger is backed by **Valkey** (shared across replicas, so the
  cap and revocation are globally consistent). The same connection
  pool as the rate limiter is reused. A per-replica in-memory store
  (`MemorySessionStore`) exists for tests and as a degraded fallback.

### Storage layout (Valkey)

| Key | Type | TTL | Purpose |
|-----|------|-----|---------|
| `sess:<sid>` | string(JSON) | idle window | session record |
| `usess:<userKey>` | set | idle window | index of a user's live sids |
| `srevoked:<sid>` | string | revoke window (24h) | revocation tombstone |

The per-session TTL gives idle expiry for free; `List` reconciles the
index set against surviving `sess:` keys.

## HTTP API

Both routes sit behind the normal OIDC auth wrapper. Identity is taken
from the authenticated context — **never from the request body** — so
a user can only ever see and revoke their own sessions.

### `GET /api/v1/sessions`

```json
{
  "sessions": [
    {
      "id": "9f86d081884c7d65...",
      "tenant_id": "…", "user_id": "…",
      "user_agent": "Mozilla/5.0 …",
      "ip": "203.0.113.7",
      "created_at": "2026-06-05T15:00:00Z",
      "last_seen":  "2026-06-05T15:42:00Z",
      "current": true
    }
  ],
  "max_concurrent": 5,
  "idle_timeout_seconds": 28800
}
```

### `POST /api/v1/sessions/revoke`

```json
{ "session_id": "9f86d081…" }      // revoke one session
{ "all_others": true }              // sign out everywhere except the caller
```

Returns `{ "revoked": ["<sid>", …] }`.

## Configuration

| Env var | Default | Meaning |
|---------|---------|---------|
| `KMAIL_SESSION_ENABLED` | `false` | Enforce the cap + revocation in the request path. When false the middleware is a passthrough, but the list/revoke API still works. |
| `KMAIL_SESSION_IDLE_TIMEOUT` | `8h` | Idle window before a session drops off the active list. |
| `KMAIL_SESSION_MAX_CONCURRENT` | `5` | Max live sessions per user; oldest evicted past this. |

### Rollout (phased)

Enforcement defaults **off** so deployments opt in deliberately. When
enabled, enforcement currently runs on the authenticated data-plane
(the JMAP proxy via `wrapAuthRL`) and on the session API itself. The
ledger is populated and the revocation/list API is fully functional
regardless. Extending enforcement to wrap every REST handler is a
follow-up: it requires threading the session middleware through the
shared `authMW.Wrap` call sites and is intentionally deferred so this
change stays additive and low-risk.

### Failure posture

The revocation check and cap enforcement **fail open** on a Valkey
error: a transient store blip must never lock every user out, and the
JWT `exp` still bounds the token. A revoked token is refused only
while its tombstone is live (24h, comfortably longer than the access
token lifetime).
