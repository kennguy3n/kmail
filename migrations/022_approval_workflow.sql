-- KMail — Phase 5: Admin access approval workflow.

BEGIN;

CREATE TABLE approval_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    requester_id    TEXT NOT NULL,
    action          TEXT NOT NULL,
    target_resource TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    approver_id     TEXT,
    reason          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '7 days')
);

CREATE INDEX approval_requests_tenant_status_idx
    ON approval_requests (tenant_id, status);

ALTER TABLE approval_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY approval_requests_isolation ON approval_requests
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE approval_config (
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    action          TEXT NOT NULL,
    requires_approval BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, action)
);

CREATE TRIGGER approval_config_set_updated_at
    BEFORE UPDATE ON approval_config
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE approval_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY approval_config_isolation ON approval_config
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
