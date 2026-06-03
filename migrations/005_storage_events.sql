-- ================================================================
-- 005_storage_events.sql
-- ----------------------------------------------------------------
-- Phase: gap closure — event-driven storage accounting (Session 4).
--
-- `storage_events` is the append-only event source for per-tenant
-- storage usage. zk-object-fabric emits S3-compatible object
-- lifecycle notifications (s3:ObjectCreated:* / s3:ObjectRemoved:*)
-- to the billing webhook, which records one row per event here.
-- The StorageEventWorker reconciles each tenant's usage by summing
-- created minus deleted bytes and writing the result into
-- `quotas.storage_used_bytes`.
--
-- RLS policy mirrors `search_cutover_jobs` (permissive on an unset
-- GUC): the reconciliation worker iterates EVERY tenant with the
-- control-plane GUC unset, so the policy must allow a NULL / empty
-- `app.tenant_id` while still isolating any stray tenant-scoped
-- session to its own rows.
--
-- Idempotent: guarded with IF NOT EXISTS. Additive only.
-- ================================================================

BEGIN;

CREATE TABLE IF NOT EXISTS storage_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_type  TEXT NOT NULL
                CHECK (event_type IN ('object_created', 'object_deleted')),
    object_key  TEXT NOT NULL DEFAULT '',
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS storage_events_tenant_created_idx
    ON storage_events (tenant_id, created_at DESC);

ALTER TABLE storage_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY storage_events_tenant_isolation ON storage_events
    USING (
        current_setting('app.tenant_id', true) IS NULL
        OR current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.tenant_id', true) IS NULL
        OR current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    );

COMMIT;
