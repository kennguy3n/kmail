-- KMail — Phase 5: Tenant data export / eDiscovery jobs.

BEGIN;

CREATE TABLE export_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    requester_id    TEXT NOT NULL,
    format          TEXT NOT NULL CHECK (format IN ('mbox', 'eml', 'pst_stub')),
    scope           TEXT NOT NULL DEFAULT 'all'
                    CHECK (scope IN ('all', 'mailbox', 'date_range')),
    scope_ref       TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    download_url    TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ
);

CREATE INDEX export_jobs_tenant_status_idx
    ON export_jobs (tenant_id, status, created_at DESC);

ALTER TABLE export_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY export_jobs_isolation ON export_jobs
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
