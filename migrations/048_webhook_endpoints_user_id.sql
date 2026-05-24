-- KMail — Phase E #15-17 follow-up: per-user webhook grants.
--
-- BACKGROUND
-- ----------
-- migrations/047_oauth2_integrations.sql added
-- `webhook_endpoints.oauth_client_id` so we knew WHICH OAuth2
-- client owned each integration-owned row. We did NOT, however,
-- track WHICH consenting user authorised that subscription, so
-- the dispatcher had to source granted-scopes from
-- `oauth_clients.allowed_scopes` (the static, client-level
-- ALLOWED set) at fire time. That column reflects which scopes
-- the client *may request*, not which scopes the user actually
-- *granted* at the consent screen — they diverge whenever:
--
--   1. A user grants fewer scopes than the client requested
--      (RFC 6749 §3.3 explicitly allows the AS to issue narrower
--      scope than requested).
--   2. A user later revokes a scope via the consent UI (deleting
--      the relevant `oauth_access_tokens` row). The client's
--      `allowed_scopes` is unchanged; the user's grant is gone.
--
-- In both cases the round-1 dispatcher kept delivering events
-- the user no longer consented to — a real privacy/scope leak
-- that Devin Review flagged as the architectural finding on PR
-- #36. Per-user grant tracking is what closes that gap.
--
-- DESIGN
-- ------
-- The user's CURRENT granted-scopes set is already first-class
-- in the schema: it is the union of `scopes` over all
-- non-revoked, non-expired `oauth_access_tokens` rows for
-- (tenant_id, client_id, user_id). When the user revokes via
-- /oauth/revoke or the consent UI, those rows flip
-- `revoked_at` IS NOT NULL and the union shrinks. A separate
-- `oauth_user_grants` table would be a second source of truth
-- subject to drift — instead we add the missing `user_id`
-- linkage to webhook_endpoints and let the dispatcher run the
-- intersection at fire time. The query plan is bounded (one
-- index lookup per integration subscriber per event) and the
-- token table is already indexed on (user_id) by 046.
--
-- Schema additions:
--   * webhook_endpoints.user_id — the consenting user (NULL for
--     legacy / admin-owned rows preserved by 047). Cascades on
--     user deletion so removing a user revokes every webhook
--     they ever subscribed an integration to.
--   * Composite index (tenant_id, oauth_client_id, user_id) so
--     the dispatcher's per-event join is index-only.
--
-- ROLLBACK
-- --------
-- Reverting this migration leaves rows with a stale user_id
-- column the application no longer reads; in practice we ship a
-- DROP COLUMN in a follow-up. Downgrading from this revision
-- means the dispatcher re-reads `oauth_clients.allowed_scopes`,
-- which restores the round-1 over-delivery behaviour but does
-- not corrupt data. Hence no DOWN script.

BEGIN;

ALTER TABLE webhook_endpoints
    ADD COLUMN IF NOT EXISTS user_id UUID
        REFERENCES users(id) ON DELETE CASCADE;

-- The dispatcher's per-event hot path:
--   SELECT … FROM webhook_endpoints we
--   WHERE we.tenant_id = $1 AND we.oauth_client_id IS NOT NULL
--     AND (we.events ? $2 OR jsonb_array_length(we.events) = 0)
--   …
-- followed by a JOIN to oauth_access_tokens on (tenant, client,
-- user). The composite index below makes the WHERE-clause
-- scan index-only and matches the JOIN key order.
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_tenant_client_user
    ON webhook_endpoints (tenant_id, oauth_client_id, user_id)
    WHERE oauth_client_id IS NOT NULL;

COMMIT;
