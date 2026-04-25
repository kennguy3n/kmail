-- KMail — Phase 4: Per-tenant zk-object-fabric storage credentials.
--
-- One row per tenant. Stores the dedicated bucket name plus the
-- per-tenant API key issued by the zk-object-fabric console
-- (`POST /api/tenants/{id}/keys`) and a reference to the tenant's
-- placement policy (`PUT /api/tenants/{id}/placement`).
--
-- The secret_key column holds the AES-encrypted secret returned by
-- the console. Phase 4 stores it verbatim (the gateway is in the
-- same trust boundary as the BFF); KMS-wrapped storage is a Phase 5
-- improvement tracked under "Customer-managed keys".

BEGIN;

CREATE TABLE tenant_storage_credentials (
    tenant_id              UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    bucket_name            TEXT NOT NULL UNIQUE,
    access_key             TEXT NOT NULL,
    encrypted_secret_key   TEXT NOT NULL,
    placement_policy_ref   TEXT NOT NULL DEFAULT '',
    encryption_mode_default TEXT NOT NULL DEFAULT 'managed'
                           CHECK (encryption_mode_default IN ('managed', 'client_side', 'public_distribution')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER tenant_storage_credentials_set_updated_at
    BEFORE UPDATE ON tenant_storage_credentials
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE tenant_storage_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_storage_credentials_isolation ON tenant_storage_credentials
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
