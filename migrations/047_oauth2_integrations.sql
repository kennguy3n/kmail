-- KMail — Phase E #15-17: third-party integrations framework.
--
-- Builds on top of:
--   * migrations/032_webhooks.sql — tenant-owned outbound webhooks.
--     Webhooks may now ALSO be owned by a registered OAuth2 client
--     (created via `internal/oauth`), in which case the dispatcher
--     filters events to scopes the client was actually granted by
--     the consenting user. Owner-NULL keeps the legacy semantics
--     (admin-owned, no scope filter).
--   * migrations/046_oauth_clients.sql — OAuth2 server. The
--     `oauth_clients` table is the source of truth for any
--     integration's identity, secret, redirect URIs, and the
--     scope set it is allowed to request. Adding an owner column
--     here keeps that single source of truth.
--
-- Why two changes in one migration?
-- The integrations framework is two halves of the same feature:
--   (a) An OAuth2 client can REGISTER a webhook to receive events
--       (the new owner column).
--   (b) A registered webhook DELIVERY is rate-limited per-client
--       so a single rogue / runaway integration cannot drown out
--       other tenants' deliveries (the new
--       `oauth_client_dispatch_quota` column).
-- Both halves are useless without the other; coupling them in one
-- migration keeps the deploy story coherent and makes downgrade
-- trivial (one DROP + one ALTER).

BEGIN;

-- ----------------------------------------------------------------
-- 1. webhook_endpoints.oauth_client_id
-- ----------------------------------------------------------------
--
-- NULL = legacy / admin-owned webhook (the tenant's own
--        operations team registered it; events are filtered by
--        the existing `events` JSONB column but NOT by OAuth2
--        scopes). This matches the pre-Phase-E behaviour.
--
-- NON-NULL = integration-owned. The dispatcher MUST refuse to
--            deliver any event whose required scope (see
--            internal/integrations.EventScope) is not in the
--            owning client's granted-scopes set at the time the
--            access token was issued. This is the security-
--            critical part: a third-party app that the user
--            granted read:mail must NOT receive
--            calendar.event_created events even if it tries to
--            subscribe to them — the SUBSCRIBE call will return
--            403 insufficient_scope, but defence in depth says
--            the dispatcher checks again at fire time in case
--            the client's scope set changed between subscribe
--            and delivery.
--
-- ON DELETE CASCADE: when a tenant administrator revokes an
-- OAuth2 client (DELETE FROM oauth_clients WHERE id = $1), every
-- webhook the integration registered goes away too. Without this
-- the rows would dangle and the dispatcher would still try to
-- deliver to a URL the integration can no longer be held
-- accountable for.
ALTER TABLE webhook_endpoints
    ADD COLUMN IF NOT EXISTS oauth_client_id UUID
        REFERENCES oauth_clients(id) ON DELETE CASCADE;

-- The list-webhooks-for-current-client query
-- (`WHERE tenant_id = $1 AND oauth_client_id = $2`) runs on every
-- GET /api/v1/integ/webhooks call.
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant_oauth_client
    ON webhook_endpoints (tenant_id, oauth_client_id)
    WHERE oauth_client_id IS NOT NULL;

-- ----------------------------------------------------------------
-- 2. oauth_clients.dispatch_quota_per_hour
-- ----------------------------------------------------------------
--
-- Per-integration sliding-window cap on outbound webhook
-- deliveries. The dispatcher reads this column when computing
-- the Valkey rate-limit key for each delivery; when the bucket
-- is full, the delivery is queued with `next_retry_at = next
-- window boundary` rather than dropped, so a temporarily noisy
-- event source doesn't lose data — it just spreads the load.
--
-- NULL means "use the global default" (Service.DefaultClientDispatchPerHour).
-- A tenant administrator who needs to throttle a misbehaving
-- integration can set a low value here without changing the
-- service-wide ceiling.
ALTER TABLE oauth_clients
    ADD COLUMN IF NOT EXISTS dispatch_quota_per_hour INTEGER
        CHECK (dispatch_quota_per_hour IS NULL OR dispatch_quota_per_hour > 0);

COMMIT;
