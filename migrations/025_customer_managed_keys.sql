-- KMail — Phase 5: Customer-managed keys (CMK).
--
-- Tenants on the privacy plan can register a public key that wraps
-- the KMail-side data encryption keys. Only the public key (PEM)
-- and a fingerprint are stored; the private key never leaves the
-- customer's HSM / KMS. Rotation deprecates the previous active
-- key but keeps it readable for re-wrap during the migration
-- window; revocation marks the key terminally unusable.

BEGIN;

CREATE TABLE customer_managed_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_fingerprint TEXT NOT NULL UNIQUE,
    public_key_pem  TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'deprecated', 'revoked')),
    algorithm       TEXT NOT NULL DEFAULT 'RSA-OAEP-256',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX customer_managed_keys_tenant_idx
    ON customer_managed_keys (tenant_id, status);

CREATE TRIGGER customer_managed_keys_set_updated_at
    BEFORE UPDATE ON customer_managed_keys
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE customer_managed_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY customer_managed_keys_isolation ON customer_managed_keys
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
