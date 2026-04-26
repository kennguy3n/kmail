-- KMail — Phase 6: BYOC HSM for Customer-Managed Keys.
--
-- Privacy-plan tenants who want their CMK material to live inside
-- a hardware security module (KMIP-speaking appliance or PKCS#11
-- token) register a connection profile here instead of uploading
-- a raw PEM. The connection credentials are stored encrypted at
-- rest using the existing kmail-secrets envelope.

BEGIN;

CREATE TABLE cmk_hsm_configs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider_type         TEXT NOT NULL CHECK (provider_type IN ('kmip', 'pkcs11')),
    endpoint              TEXT NOT NULL,
    slot_id               TEXT NOT NULL DEFAULT '',
    credentials_encrypted BYTEA NOT NULL,
    status                TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'active', 'failed', 'revoked')),
    last_test_at          TIMESTAMPTZ,
    last_test_error       TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX cmk_hsm_configs_tenant_idx ON cmk_hsm_configs (tenant_id);

CREATE TRIGGER cmk_hsm_configs_set_updated_at
    BEFORE UPDATE ON cmk_hsm_configs
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE cmk_hsm_configs ENABLE ROW LEVEL SECURITY;
CREATE POLICY cmk_hsm_configs_isolation ON cmk_hsm_configs
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
