-- KMail — Phase 5: auto-cutover from Meilisearch to OpenSearch.
--
-- The `cutover_state` machine tracks each tenant through a
-- migration so a worker crash mid-reindex resumes the same job on
-- restart instead of double-flipping the backend or losing track
-- of the bulk import. The states are linear:
--
--   pending      eligible (size >= threshold) but not yet started
--   in_progress  Reindex is running; do not retrigger
--   completed    Backend flipped, reindex done
--   failed       Reindex errored; the worker retries on the next
--                tick using `failure_count` as a back-off hint
--
-- The single-row-per-tenant shape lets the worker idempotently
-- claim a tenant via `UPDATE ... WHERE cutover_state = 'pending'`
-- so two replicas racing the same tick land on a unique winner.

BEGIN;

CREATE TABLE IF NOT EXISTS search_cutover_jobs (
    tenant_id      UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    cutover_state  TEXT NOT NULL DEFAULT 'pending'
        CHECK (cutover_state IN ('pending', 'in_progress', 'completed', 'failed')),
    mailbox_size   BIGINT NOT NULL DEFAULT 0,
    threshold      BIGINT NOT NULL,
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    failure_count  INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS search_cutover_jobs_state_idx
    ON search_cutover_jobs (cutover_state, updated_at);

ALTER TABLE search_cutover_jobs ENABLE ROW LEVEL SECURITY;
-- The auto-cutover worker runs with the control-plane GUC unset
-- (it iterates every tenant), so a permissive policy is required
-- here. RLS is still enabled so a stray tenant-scoped session
-- can't read another tenant's row.
CREATE POLICY search_cutover_jobs_tenant_isolation ON search_cutover_jobs
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
