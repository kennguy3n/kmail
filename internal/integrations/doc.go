// Package integrations is the third-party integration framework
// for KMail. It sits atop the OAuth2 server in `internal/oauth`
// and the webhook delivery machinery in `internal/webhooks`,
// adding the per-client scope enforcement and rate-limited
// dispatch that production integration shipping requires.
//
// # Surface
//
// Each handler is authenticated by an OAuth2 bearer token issued
// by the authorization server (see internal/oauth/handlers.go).
// The framework exposes:
//
//   - POST   /api/v1/integ/webhooks          — register a webhook
//     for the calling OAuth2 client. The set of event types the
//     client subscribes to is filtered through
//     FilterEventsForClient at the boundary: any event the client
//     was not granted a scope for is rejected with a structured
//     422 (so the integration learns which scope it is missing
//     and can re-prompt the user for consent).
//   - GET    /api/v1/integ/webhooks          — list webhooks the
//     calling client has registered. Scoped to
//     `tenant_id = <ctx tenant> AND oauth_client_id = <calling client>`
//     so an integration cannot enumerate other integrations'
//     subscriptions.
//   - DELETE /api/v1/integ/webhooks/{id}     — delete a webhook
//     the calling client owns. Same scoping as list.
//   - POST   /api/v1/integ/webhooks/{id}/test — fire a synthetic
//     `webhook.ping` delivery so an integration can verify the
//     receiver URL is up.
//
// # Scope enforcement (defence in depth)
//
// The framework enforces scopes at three independent points so a
// missing check in one layer doesn't open a privilege-escalation
// path:
//
//  1. Subscribe-time. RegisterWebhookForClient filters the
//     requested event list through FilterEventsForClient and
//     refuses (422) if every requested event was denied.
//  2. List / delete time. SQL queries scope by oauth_client_id
//     so a client never sees a row another client owns.
//  3. Dispatch time. The Dispatcher consults
//     EventAllowedForClient before INSERTing into
//     webhook_deliveries. A client whose granted scopes shrunk
//     between subscribe-time and dispatch-time (e.g. the user
//     re-consented with fewer scopes, or the operator revoked
//     a scope) stops receiving events on the very next fire.
//
// # Per-client rate-limited dispatch
//
// Each OAuth2 client gets a sliding-window outbound-delivery
// quota. The default is taken from
// Service.DefaultClientDispatchPerHour at construction; an
// operator can override per-client by setting
// oauth_clients.dispatch_quota_per_hour (migration 047). Quota
// is enforced via a Valkey INCR+EXPIRE bucket keyed on
// (oauth_client_id, hourly bucket); on overflow the dispatcher
// returns ErrClientQuotaExceeded so the caller can:
//
//   - drop the event (high-volume firehose case), or
//   - persist it with `next_retry_at = next window boundary`
//     (the framework's at-least-once default).
//
// The fail-open behaviour matches internal/middleware/ratelimit:
// a transient Valkey error allows the delivery through and is
// logged, so an outage of the rate limiter never silently
// blocks legitimate integrations.
//
// # Why a separate package from webhooks
//
// `internal/webhooks` is the underlying delivery machinery — the
// HMAC signing, the worker queue, the retry/backoff loop. That
// package has no notion of OAuth2 clients, and shouldn't:
// embedding client-scoped rate-limit and scope filtering in the
// delivery worker would couple it to OAuth2 server changes every
// time a new scope is introduced. integrations is a thin layer
// that:
//
//   - Owns the OAuth2-scoped HTTP surface.
//   - Translates `(oauth_client_id, event_type)` → "ok / refused"
//     before any row gets enqueued.
//   - Wraps webhooks.Service for the actual register / delete /
//     test-fire primitives, adding the oauth_client_id column
//     scoping in custom SQL where webhooks.Service does not
//     surface client identity in its API.
//
// # Polling triggers
//
// Zapier-compatible polling triggers
// (GET /api/v1/integ/triggers/{name}) are scoped to a follow-up
// PR. They require either an event log with retention semantics
// distinct from webhook_deliveries (which is a per-endpoint
// queue) or a poll-only endpoint mode for webhooks. Both are
// real schema changes that deserve their own migration + review
// cycle and are intentionally not stubbed here.
package integrations
