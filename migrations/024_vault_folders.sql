-- KMail — Phase 5: Zero-Access Vault folders.
--
-- Vault folders host messages encrypted client-side with a key the
-- BFF never sees. We persist only metadata about the wrapping —
-- the wrapped DEK blob (already encrypted under the user's MLS
-- credential / KChat key tree), the wrapping algorithm, and the
-- nonce. Plaintext keys never enter the table; the server cannot
-- search the contents. See `docs/PROGRESS.md` Phase 5 §Zero-Access
-- Vault and the privacy-mode ↔ zk-object-fabric mode mapping in
-- `docs/PROPOSAL.md` (StrictZK).

BEGIN;

CREATE TABLE vault_folders (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL,
    folder_name     TEXT NOT NULL,
    encryption_mode TEXT NOT NULL DEFAULT 'StrictZK'
                    CHECK (encryption_mode IN ('StrictZK')),
    wrapped_dek     BYTEA NOT NULL DEFAULT ''::bytea,
    key_algorithm   TEXT NOT NULL DEFAULT 'XChaCha20-Poly1305',
    nonce           BYTEA NOT NULL DEFAULT ''::bytea,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX vault_folders_tenant_user_idx
    ON vault_folders (tenant_id, user_id, folder_name);

CREATE TRIGGER vault_folders_set_updated_at
    BEFORE UPDATE ON vault_folders
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE vault_folders ENABLE ROW LEVEL SECURITY;
CREATE POLICY vault_folders_isolation ON vault_folders
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
