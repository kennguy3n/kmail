-- KMail — Scheduled Send (WS4).
--
-- Stores future EmailSubmission/set payloads that the worker
-- dispatches to Stalwart once `send_at` falls into the past.
-- Modelled on `webhook_deliveries` (migration 032): a single
-- queue table with `pending → sent | cancelled | failed` lifecycle,
-- a partial index on the pending hot path so a long tail of
-- delivered rows doesn't bloat the worker's claim scan, RLS by
-- `app.tenant_id`, and an `updated_at` trigger.
--
-- Why a Postgres queue instead of Valkey (compare with the WS3
-- Undo-Send hold queue):
--
--   * Persistence horizon. Undo Send holds for <30s; losing a hold
--     on a Valkey restart is acceptable (the user simply doesn't
--     get the cancel window). Scheduled Send holds for minutes →
--     weeks; the data is real durable user intent and must survive
--     every restart and replication failover.
--
--   * Auditability. "Show me every email this user has scheduled
--     in the next 30 days" must be a single indexed query for the
--     admin console + the user's Scheduled view. Valkey would
--     require maintaining a parallel inventory.
--
--   * Cancellation surface. The user can navigate away and cancel
--     from a different device days later. Tying the cancel path
--     to a Valkey TTL would force operators into TTL extension
--     gymnastics. SQL UPDATE WHERE status='pending' is trivial.
--
-- Tenant isolation is enforced by RLS. The worker runs without a
-- tenant GUC (it must cross-tenant scan to find due rows) and
-- relies on the BFF role being exempt from forced RLS — the same
-- model used by `webhook_deliveries` and `search_cutover_jobs`.

BEGIN;

CREATE TABLE scheduled_sends (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    kchat_user_id  TEXT NOT NULL,
    stalwart_account_id TEXT NOT NULL,
    email_id       TEXT NOT NULL,        -- JMAP Email id (the draft)
    identity_id    TEXT NOT NULL,        -- JMAP Identity id
    submission     JSONB NOT NULL,       -- serialized EmailSubmission/set create args
    send_at        TIMESTAMPTZ NOT NULL, -- when the worker should dispatch
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'sent', 'cancelled', 'failed')),
    attempts       INT NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    next_retry_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Worker hot path: claim the next pending row whose send_at has
-- elapsed AND whose retry window has elapsed (after a transient
-- Stalwart failure the row stays `pending` with `next_retry_at`
-- pushed into the future). Partial index keeps the index small
-- even when the queue contains a long tail of `sent` rows kept
-- for audit / the user-facing "Scheduled" view.
CREATE INDEX scheduled_sends_pending_idx
    ON scheduled_sends (send_at, next_retry_at)
    WHERE status = 'pending';

-- User-facing "List scheduled" query: tenant + user + status,
-- newest first. Same shape as the `mailboxes` per-tenant index.
CREATE INDEX scheduled_sends_tenant_user_idx
    ON scheduled_sends (tenant_id, kchat_user_id, created_at DESC);

CREATE TRIGGER scheduled_sends_set_updated_at
    BEFORE UPDATE ON scheduled_sends
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE scheduled_sends ENABLE ROW LEVEL SECURITY;
CREATE POLICY scheduled_sends_tenant_isolation
    ON scheduled_sends
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
