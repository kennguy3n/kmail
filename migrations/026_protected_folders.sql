-- KMail — Phase 5: Protected folders + sharing.
--
-- Protected folders are server-managed encrypted folders (using
-- the standard ManagedEncrypted envelope) that an owner can share
-- with a small number of teammates inside the same tenant. Sharing
-- is recorded explicitly so the owner can revoke; every grant /
-- revoke / read-as-grantee operation appends to an access log.
--
-- This is distinct from `vault_folders` (Zero-Access Vault, no
-- server search): protected folders keep server-side scanning and
-- spam filtering — they only restrict who can _open_ them.

BEGIN;

CREATE TABLE protected_folders (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    owner_id        TEXT NOT NULL,
    folder_name     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX protected_folders_tenant_owner_idx
    ON protected_folders (tenant_id, owner_id);

CREATE TRIGGER protected_folders_set_updated_at
    BEFORE UPDATE ON protected_folders
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE protected_folders ENABLE ROW LEVEL SECURITY;
CREATE POLICY protected_folders_isolation ON protected_folders
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE protected_folder_access (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    folder_id       UUID NOT NULL REFERENCES protected_folders(id) ON DELETE CASCADE,
    grantee_id      TEXT NOT NULL,
    permission      TEXT NOT NULL DEFAULT 'read'
                    CHECK (permission IN ('read', 'read_write')),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (folder_id, grantee_id)
);

CREATE INDEX protected_folder_access_tenant_idx
    ON protected_folder_access (tenant_id, folder_id);

ALTER TABLE protected_folder_access ENABLE ROW LEVEL SECURITY;
CREATE POLICY protected_folder_access_isolation ON protected_folder_access
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE protected_folder_access_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    folder_id       UUID NOT NULL REFERENCES protected_folders(id) ON DELETE CASCADE,
    actor_id        TEXT NOT NULL,
    action          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX protected_folder_access_log_idx
    ON protected_folder_access_log (tenant_id, folder_id, created_at DESC);

ALTER TABLE protected_folder_access_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY protected_folder_access_log_isolation ON protected_folder_access_log
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
