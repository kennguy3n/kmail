-- KMail — Phase 5: Retention enforcement results.
--
-- Each retention worker tick that actually evaluates a policy
-- writes a row here so admins can audit how many messages were
-- deleted / archived per run. Dry-run executions are recorded
-- with `emails_deleted = 0` and a `dry_run = true` marker in
-- `notes` so the admin can see what *would* have happened
-- before flipping the kill-switch.

BEGIN;

CREATE TABLE retention_enforcement_log (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    policy_id         UUID NOT NULL REFERENCES retention_policies(id) ON DELETE CASCADE,
    emails_processed  INT NOT NULL DEFAULT 0,
    emails_deleted    INT NOT NULL DEFAULT 0,
    emails_archived   INT NOT NULL DEFAULT 0,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    error             TEXT NOT NULL DEFAULT '',
    notes             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX retention_enforcement_tenant_idx
    ON retention_enforcement_log (tenant_id, started_at DESC);
CREATE INDEX retention_enforcement_policy_idx
    ON retention_enforcement_log (policy_id);

ALTER TABLE retention_enforcement_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY retention_enforcement_isolation ON retention_enforcement_log
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
