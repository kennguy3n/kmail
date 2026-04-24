-- KMail — admin audit log (Phase 3).
--
-- Every row is a single administrative action (tenant.update,
-- user.delete, domain.verify, etc.). `prev_hash` chains rows in
-- creation order so the table is tamper-evident — see
-- docs/PROGRESS.md Phase 3 "Admin audit logs" entry and the hash
-- computation in internal/audit/audit.go.

BEGIN;

CREATE TABLE audit_log (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_id       TEXT NOT NULL,
    actor_type     TEXT NOT NULL CHECK (actor_type IN ('user', 'system', 'admin')),
    action         TEXT NOT NULL,
    resource_type  TEXT NOT NULL,
    resource_id    TEXT,
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip_address     INET,
    user_agent     TEXT,
    prev_hash      TEXT NOT NULL DEFAULT '',
    entry_hash     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_time_idx
    ON audit_log (tenant_id, created_at DESC);

CREATE INDEX audit_log_action_idx
    ON audit_log (tenant_id, action, created_at DESC);

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

CREATE POLICY audit_log_tenant_isolation
    ON audit_log
    USING (tenant_id::text = current_setting('app.tenant_id', true));

COMMIT;
