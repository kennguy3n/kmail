-- KMail — Phase 8: TOTP fallback for WebAuthn.
--
-- PROPOSAL.md §10.1 specifies "TOTP as fallback" alongside FIDO2.
-- One row per (tenant, user). The TOTP shared secret is stored
-- wrapped by the kmail-secrets AEAD envelope — never plaintext.
-- Recovery codes are stored as bcrypt hashes (one per code,
-- pipe-delimited) so a database leak does not yield usable codes.
-- `enabled` flips true once the user verifies the first TOTP
-- code during enrolment; pre-verify rows count as "in setup".

BEGIN;

CREATE TABLE IF NOT EXISTS totp_credentials (
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id              TEXT NOT NULL,
    encrypted_secret     BYTEA NOT NULL,
    recovery_codes_hash  TEXT NOT NULL DEFAULT '',
    enabled              BOOLEAN NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at         TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX IF NOT EXISTS totp_credentials_user_idx
    ON totp_credentials (tenant_id, user_id) WHERE enabled = TRUE;

ALTER TABLE totp_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY totp_credentials_isolation ON totp_credentials
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
