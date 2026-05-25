-- KMail — Email Snooze (WS5).
--
-- A snooze row holds the original mailbox membership of an
-- already-delivered email while the message is hidden in a
-- per-user "Snoozed" mailbox. When `snooze_until` falls into the
-- past, the worker moves the email back to its original
-- mailboxes, marks the row `unsnoozed`, and (optionally) clears
-- the seen flag so the email surfaces as new again.
--
-- Modelled on `scheduled_sends` (migration #051):
--   * One queue table with a clean status lifecycle
--     (`snoozed → unsnoozed | cancelled`); a `failed` terminal
--     state is reserved for cases where Stalwart rejects the
--     mailbox move even after retries.
--   * Partial index on the pending hot path so a long tail of
--     `unsnoozed` rows kept for audit doesn't bloat the worker
--     scan.
--   * RLS by `app.tenant_id`. The worker runs without a tenant
--     GUC (cross-tenant scan to find due rows) and relies on the
--     BFF role being exempt from forced RLS — same model used
--     by `webhook_deliveries`, `alias_stalwart_sync_queue`, and
--     `scheduled_sends`.
--
-- Why Postgres, not Valkey:
--   * Persistence horizon. Snoozes are hours → weeks; the data
--     is durable user intent that must survive every restart.
--   * Tenant-side audit. "Show me every snooze for this user in
--     the last 30 days" is a single indexed SELECT — Valkey
--     would require maintaining a parallel inventory.
--   * Cross-device cancellation. The user can wake up an email
--     from a different device days later; SQL UPDATE WHERE
--     status='snoozed' is trivial.
--
-- `original_mailbox_ids` stores the JMAP `mailboxIds` map the
-- email lived in at snooze time, serialised as a JSON object of
-- the form `{"mb-inbox": true, "mb-imp": true}` so the worker
-- can restore the exact set on wake.

BEGIN;

CREATE TABLE snoozed_emails (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    kchat_user_id        TEXT NOT NULL,
    stalwart_account_id  TEXT NOT NULL,
    email_id             TEXT NOT NULL,            -- JMAP Email id
    original_mailbox_ids JSONB NOT NULL,           -- {"mb-inbox": true, ...}
    snoozed_mailbox_id   TEXT NOT NULL,            -- where the email lives while snoozed
    snooze_until         TIMESTAMPTZ NOT NULL,     -- when the worker should wake the email
    mark_unread_on_wake  BOOLEAN NOT NULL DEFAULT TRUE,
    status               TEXT NOT NULL DEFAULT 'snoozed'
                         CHECK (status IN ('snoozed', 'unsnoozed', 'cancelled', 'failed')),
    attempts             INT NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    next_retry_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    woken_at             TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Worker hot path: claim the next snoozed row whose snooze_until
-- has elapsed AND whose retry window has elapsed. Partial index
-- keeps the index small even when the queue contains a long
-- tail of `unsnoozed` rows kept for audit / user-facing view.
CREATE INDEX snoozed_emails_pending_idx
    ON snoozed_emails (snooze_until, next_retry_at)
    WHERE status = 'snoozed';

-- User-facing "Show my snoozed" query: tenant + user + status,
-- newest first.
CREATE INDEX snoozed_emails_tenant_user_idx
    ON snoozed_emails (tenant_id, kchat_user_id, created_at DESC);

-- One snooze per (tenant, email) at a time. A second snooze on
-- an already-snoozed email is almost always a UI race — the
-- caller should cancel the existing row first. Enforced as a
-- unique partial index on the active states only so an old
-- `unsnoozed` row doesn't block a future snooze of the same
-- email.
CREATE UNIQUE INDEX snoozed_emails_active_unique
    ON snoozed_emails (tenant_id, email_id)
    WHERE status = 'snoozed';

CREATE TRIGGER snoozed_emails_set_updated_at
    BEFORE UPDATE ON snoozed_emails
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE snoozed_emails ENABLE ROW LEVEL SECURITY;
CREATE POLICY snoozed_emails_tenant_isolation
    ON snoozed_emails
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
