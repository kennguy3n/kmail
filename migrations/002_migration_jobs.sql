-- KMail — migration-orchestrator schema.
--
-- Owner: `internal/migration` (the Gmail / IMAP import
-- orchestrator; see `docs/ARCHITECTURE.md` §7 and `docs/PROPOSAL.md`
-- §11). One row per imapsync job; workers update `status`,
-- `progress_pct`, `messages_synced`, `started_at`, and
-- `completed_at` as the sync advances.

BEGIN;

CREATE TABLE IF NOT EXISTS migration_jobs (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    source_host                TEXT NOT NULL,
    source_user                TEXT NOT NULL,
    source_password_encrypted  TEXT,
    dest_user                  TEXT NOT NULL,
    status                     TEXT NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'running',
                                                 'paused', 'cancelled',
                                                 'failed', 'completed')),
    progress_pct               INT NOT NULL DEFAULT 0
                               CHECK (progress_pct BETWEEN 0 AND 100),
    messages_total             INT,
    messages_synced            INT,
    started_at                 TIMESTAMPTZ,
    completed_at               TIMESTAMPTZ,
    error_msg                  TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS migration_jobs_tenant_id_idx
    ON migration_jobs (tenant_id);
CREATE INDEX IF NOT EXISTS migration_jobs_status_idx
    ON migration_jobs (status)
    WHERE status IN ('pending', 'running');

-- Reuses the kmail_set_updated_at() trigger function defined in
-- migrations/001_initial_schema.sql.
DROP TRIGGER IF EXISTS migration_jobs_set_updated_at ON migration_jobs;
CREATE TRIGGER migration_jobs_set_updated_at
    BEFORE UPDATE ON migration_jobs
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE migration_jobs ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS migration_jobs_tenant ON migration_jobs;
CREATE POLICY migration_jobs_tenant ON migration_jobs
    USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

COMMIT;
