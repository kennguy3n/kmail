-- KMail — Phase 7: per-tenant Sieve rule store.
--
-- One row per Sieve rule. Tenant-wide rules carry user_id IS NULL;
-- per-user rules carry the KChat user identifier. Stalwart owns
-- execution; the BFF persists the script and pushes the enabled
-- subset on deploy.

BEGIN;

CREATE TABLE IF NOT EXISTS sieve_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     TEXT,
    name        TEXT NOT NULL,
    script      TEXT NOT NULL,
    priority    INT  NOT NULL DEFAULT 100,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sieve_rules_tenant_priority_idx
    ON sieve_rules (tenant_id, priority, created_at);

CREATE TRIGGER sieve_rules_set_updated_at
    BEFORE UPDATE ON sieve_rules
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE sieve_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY sieve_rules_isolation ON sieve_rules
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
