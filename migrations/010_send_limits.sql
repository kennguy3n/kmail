-- KMail — Phase 3: Tenant send limits.
--
-- Per-tenant daily / hourly send caps. Defaults are plan-based
-- (core=500/day, pro=2000/day, privacy=5000/day); rows override the
-- plan defaults when an operator wants to pin a specific limit.

BEGIN;

CREATE TABLE tenant_send_limits (
    tenant_id        UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    daily_limit      INT NOT NULL CHECK (daily_limit >= 0),
    hourly_limit     INT NOT NULL CHECK (hourly_limit >= 0),
    warmup_override  INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER tenant_send_limits_set_updated_at
    BEFORE UPDATE ON tenant_send_limits
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE tenant_send_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_send_limits_tenant_isolation
    ON tenant_send_limits
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
