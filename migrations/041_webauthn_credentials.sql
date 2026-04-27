-- KMail — Phase 7: WebAuthn / FIDO2 credential storage.
--
-- One row per security key per (tenant, user). Public keys are
-- stored as base64url-encoded COSE blobs the browser hands us at
-- registration time. `sign_count` tracks the authenticator's
-- signature counter so cloned-credential abuse can be detected
-- (sign_count must monotonically increase across assertions).

BEGIN;

CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    public_key    TEXT NOT NULL,
    sign_count    BIGINT NOT NULL DEFAULT 0,
    name          TEXT NOT NULL DEFAULT 'Security key',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    UNIQUE (tenant_id, credential_id)
);

CREATE INDEX IF NOT EXISTS webauthn_credentials_user_idx
    ON webauthn_credentials (tenant_id, user_id);

ALTER TABLE webauthn_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY webauthn_credentials_isolation ON webauthn_credentials
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
