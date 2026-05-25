-- KMail — persisted retry queue for Stalwart alias sync.
--
-- The Tenant Service mirrors alias CRUD into Stalwart's principal
-- database (PATCH /api/principal/{name}). The BFF row is the source
-- of truth for the admin console; Stalwart sync is best-effort. The
-- original implementation returned the Stalwart-sync error to the
-- HTTP handler, which mapped it to 500 — that contradicts the
-- "BFF row authoritative" design and left the client with a 500
-- after a successful DB write (retry would 409 on the unique
-- constraint).
--
-- This migration introduces `alias_stalwart_sync_queue`, a row-per-
-- pending-sync table modelled on `webhook_deliveries` (migration
-- 032). The service enqueues a row inside the same transaction
-- that writes / deletes the alias, then attempts Stalwart sync
-- inline. On inline success the row is marked `synced`; on inline
-- failure the row stays `pending` for the
-- `AliasStalwartSyncWorker` to retry with exponential backoff.
--
-- Why a queue table instead of a fire-and-forget log line:
--
--   * Guarantees eventual sync after Stalwart recovers from a
--     transient outage. A logged error vanishes after log rotation.
--   * Lets operators inspect the backlog ("how far behind is
--     Stalwart on alias propagation?") via a tenant-scoped admin
--     query.
--   * Atomic enqueue inside the alias write transaction — a crash
--     between the alias INSERT and the queue INSERT is impossible.

BEGIN;

CREATE TABLE alias_stalwart_sync_queue (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    operation           TEXT NOT NULL
                        CHECK (operation IN ('add', 'remove')),
    stalwart_account_id TEXT NOT NULL,
    alias_email         TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'synced', 'failed')),
    attempts            INT NOT NULL DEFAULT 0,
    last_error          TEXT NOT NULL DEFAULT '',
    next_retry_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    synced_at           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Worker's hot path: claim the next pending row whose backoff has
-- elapsed. Partial index keeps the index small even when the queue
-- contains a long tail of `synced` rows kept for audit.
CREATE INDEX alias_stalwart_sync_queue_pending_idx
    ON alias_stalwart_sync_queue (next_retry_at)
    WHERE status = 'pending';

-- Admin query path: "show me this tenant's recent sync activity".
CREATE INDEX alias_stalwart_sync_queue_tenant_idx
    ON alias_stalwart_sync_queue (tenant_id, created_at DESC);

CREATE TRIGGER alias_stalwart_sync_queue_set_updated_at
    BEFORE UPDATE ON alias_stalwart_sync_queue
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

-- RLS scopes admin reads to the tenant whose GUC is set. The
-- worker reads across all tenants without setting the GUC, which
-- is permitted because RLS is ENABLED (not FORCED) on this table —
-- the BFF role owns the table and is therefore exempt unless we
-- explicitly FORCE it. That matches every other queue table in
-- the schema (see `webhook_deliveries`, `search_cutover_jobs`).
ALTER TABLE alias_stalwart_sync_queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY alias_stalwart_sync_queue_isolation
    ON alias_stalwart_sync_queue
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
