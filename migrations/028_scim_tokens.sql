-- KMail — Phase 5: SCIM 2.0 provisioning bearer tokens.
--
-- Each row is a tenant-scoped bearer token that an external IdP
-- (Okta, Azure AD, Google Workspace, JumpCloud) presents on
-- `/scim/v2/Users` and `/scim/v2/Groups` requests. The token is
-- hashed with SHA-256 before storage so a DB compromise does not
-- leak live credentials. Revoking is a soft-delete via
-- `revoked_at` so the audit trail survives.

BEGIN;

CREATE TABLE scim_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX scim_tokens_tenant_idx ON scim_tokens (tenant_id);
CREATE INDEX scim_tokens_active_idx ON scim_tokens (token_hash) WHERE revoked_at IS NULL;

ALTER TABLE scim_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY scim_tokens_isolation ON scim_tokens
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
