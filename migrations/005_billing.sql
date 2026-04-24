-- KMail — Phase 3: Billing / Quota Service.
--
-- Adds the `billing_events` event log + an `account_type` column on
-- `users` so shared-inbox / service accounts can be excluded from
-- seat counts without scanning address patterns. The `quotas` table
-- stood up in `001_initial_schema.sql` already carries the storage
-- and seat pool columns this service reads from.

BEGIN;

-- ---------------------------------------------------------------
-- users.account_type
-- ---------------------------------------------------------------
--
-- Phase 1 treated every row in `users` as a paid seat. Phase 3 adds
-- shared inboxes, service accounts, and other non-seat account
-- types (see Task 8 — Shared Inboxes Without Paid Seats). The
-- column defaults to `user` so existing rows keep the old meaning.

ALTER TABLE users
    ADD COLUMN account_type TEXT NOT NULL DEFAULT 'user'
        CHECK (account_type IN ('user', 'shared_inbox', 'service'));

CREATE INDEX users_tenant_account_type_idx
    ON users (tenant_id, account_type) WHERE status = 'active';

-- ---------------------------------------------------------------
-- billing_events
-- ---------------------------------------------------------------
--
-- Append-only log of everything that affects an invoice: seat
-- additions/removals, storage-usage deltas surfaced by the quota
-- worker, invoice generation, plan changes. RLS-scoped so one
-- tenant's operators can only see their own events.

CREATE TABLE billing_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    event_type     TEXT NOT NULL
                   CHECK (event_type IN (
                       'seat_added', 'seat_removed',
                       'storage_delta', 'storage_snapshot',
                       'plan_changed', 'invoice_generated',
                       'limit_adjusted'
                   )),
    seat_count     INT,
    storage_delta  BIGINT,
    amount_cents   BIGINT,
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_events_tenant_created_idx
    ON billing_events (tenant_id, created_at DESC);
CREATE INDEX billing_events_tenant_type_created_idx
    ON billing_events (tenant_id, event_type, created_at DESC);

ALTER TABLE billing_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_events_tenant_isolation ON billing_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
