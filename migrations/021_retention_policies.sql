-- KMail — Phase 5: Retention / archive policies.
--
-- Adds `retention_policies` so tenant admins can declare auto-
-- archive or auto-delete after N days. The retention worker
-- evaluates these daily.

BEGIN;

CREATE TABLE retention_policies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    policy_type     TEXT NOT NULL CHECK (policy_type IN ('archive', 'delete')),
    retention_days  INT  NOT NULL CHECK (retention_days > 0),
    applies_to      TEXT NOT NULL DEFAULT 'all' CHECK (applies_to IN ('all', 'mailbox', 'label')),
    target_ref      TEXT NOT NULL DEFAULT '',
    enabled         BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX retention_policies_tenant_idx
    ON retention_policies (tenant_id);

CREATE TRIGGER retention_policies_set_updated_at
    BEFORE UPDATE ON retention_policies
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE retention_policies ENABLE ROW LEVEL SECURITY;
CREATE POLICY retention_policies_isolation ON retention_policies
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
