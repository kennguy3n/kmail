-- 046_oauth_clients.sql — Phase E #14: OAuth2 server foundation for
-- third-party integrations (Zapier, Slack, custom apps).
--
-- KMail accepts two distinct token populations:
--
--   * OIDC user JWTs (the existing KChat-issued tokens). Verified
--     by `internal/middleware` against the JWKS at startup; this
--     is how the web/native clients authenticate human users.
--
--   * OAuth2 access tokens (this migration). Issued to third-party
--     applications via the authorization code grant; bearer-only.
--     Verified by `internal/oauth.AuthMiddleware` against the
--     `oauth_access_tokens` table on every request. Tokens are
--     scoped (read:mail, write:mail, read:calendar, etc.) so a
--     third-party app cannot exceed the scopes the user granted
--     at authorization time.
--
-- The two populations carry independent tenant_id / user_id
-- contexts: a user JWT identifies the human; an OAuth2 token
-- identifies the *third-party app acting on behalf of* the human
-- that granted consent. The downstream API surface treats both
-- equivalently for RLS purposes (the same SetTenantGUC is
-- applied), but audit log entries distinguish the two so we can
-- attribute actions correctly.

BEGIN;

-- oauth_clients: registered third-party applications. Each row
-- represents a single OAuth2 client (an application), not a user
-- session. Clients are per-tenant — a Zapier integration in
-- tenant A is a different row from the same integration in
-- tenant B, with its own client_secret hash.
--
-- redirect_uris stores the allow-list of callback URLs. The
-- /oauth/authorize handler rejects any redirect_uri that does
-- not exactly match an entry in this array (no prefix matching;
-- mismatched callback URLs are a known OAuth2 attack vector).
--
-- client_type captures whether the client is "confidential"
-- (a server-side app with a secret) or "public" (an SPA / mobile
-- app that cannot keep a secret). Public clients are required to
-- use PKCE; confidential clients may use PKCE or client_secret.
CREATE TABLE IF NOT EXISTS oauth_clients (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id         TEXT NOT NULL UNIQUE,
    -- bcrypt hash of the plaintext client_secret. Only present
    -- for confidential clients; NULL for public clients.
    client_secret_hash TEXT,
    client_type       TEXT NOT NULL CHECK (client_type IN ('confidential', 'public')),
    name              TEXT NOT NULL,
    -- Optional URL of the third-party application's homepage,
    -- shown on the consent screen so the user knows what they're
    -- granting access to.
    homepage_url      TEXT,
    -- Optional logo URL shown on the consent screen.
    logo_url          TEXT,
    redirect_uris     JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Allow-list of scopes this client can request. The actual
    -- scopes granted to a given access token are the intersection
    -- of this list, the scopes requested in the authorize URL,
    -- and the scopes the user approved on the consent screen.
    allowed_scopes    JSONB NOT NULL DEFAULT '[]'::jsonb,
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_clients_tenant ON oauth_clients(tenant_id);
ALTER TABLE oauth_clients ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_oauth_clients ON oauth_clients;
CREATE POLICY rls_oauth_clients ON oauth_clients
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- oauth_authorization_codes: one-time-use codes issued by
-- /oauth/authorize and exchanged for tokens at /oauth/token.
--
-- code_challenge / code_challenge_method enforce PKCE per
-- RFC 7636. The token endpoint verifies code_verifier against
-- this challenge on exchange.
--
-- expires_at is bounded to 60 seconds per the OAuth2 spec
-- (RFC 6749 §4.1.2) — codes are short-lived because they're
-- bearer credentials that ride over the user's browser.
--
-- consumed_at flips to non-null on first /oauth/token exchange;
-- a second exchange with the same code MUST be rejected (and
-- per the spec, the originally-issued tokens revoked).
CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id              UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id                UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- SHA-256 hex hash of the plaintext code that was sent to the
    -- browser; the plaintext exists only in the redirect URL and
    -- in the third-party app's memory.
    code_hash              TEXT NOT NULL UNIQUE,
    redirect_uri           TEXT NOT NULL,
    granted_scopes         JSONB NOT NULL DEFAULT '[]'::jsonb,
    code_challenge         TEXT,
    code_challenge_method  TEXT CHECK (code_challenge_method IN ('plain', 'S256')),
    expires_at             TIMESTAMPTZ NOT NULL,
    consumed_at            TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_codes_expires ON oauth_authorization_codes(expires_at);
ALTER TABLE oauth_authorization_codes ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_oauth_codes ON oauth_authorization_codes;
CREATE POLICY rls_oauth_codes ON oauth_authorization_codes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- oauth_access_tokens: bearer tokens issued by /oauth/token.
-- Expires within an hour by default; clients refresh via the
-- refresh_token grant. The token plaintext is never stored —
-- we keep a SHA-256 hash so a database compromise does not
-- yield valid bearer tokens.
--
-- revoked_at supports the /oauth/revoke endpoint and the
-- automatic revocation triggered by code-reuse (RFC 6819).
CREATE TABLE IF NOT EXISTS oauth_access_tokens (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id       UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      TEXT NOT NULL UNIQUE,
    scopes          JSONB NOT NULL DEFAULT '[]'::jsonb,
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The refresh_token row that this access token is bound to,
    -- so revoking a refresh token cascades to all access tokens
    -- it ever issued (rotation safety).
    refresh_token_id UUID
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_expires ON oauth_access_tokens(expires_at);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_user ON oauth_access_tokens(user_id);
ALTER TABLE oauth_access_tokens ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_oauth_tokens ON oauth_access_tokens;
CREATE POLICY rls_oauth_tokens ON oauth_access_tokens
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- oauth_refresh_tokens: long-lived tokens used to mint new
-- access tokens without re-prompting the user. Subject to
-- refresh-token rotation: each /oauth/token call with a
-- refresh_token grant invalidates the old refresh token and
-- issues a new one, so a stolen refresh token is detectable
-- (the original owner's next use will fail, triggering full
-- revocation of the token family).
CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id    UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The previous refresh-token row that this token was rotated
    -- from. NULL for the first refresh token in a chain (issued
    -- alongside the initial code exchange). On rotation, the
    -- previous row is marked revoked and the new row references
    -- it via this column. If a /oauth/token call presents a
    -- refresh_token that is already revoked AND has a successor,
    -- that's a replay attempt — revoke the entire successor
    -- chain.
    parent_id    UUID,
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_refresh_expires ON oauth_refresh_tokens(expires_at);
CREATE INDEX IF NOT EXISTS idx_oauth_refresh_parent ON oauth_refresh_tokens(parent_id);
ALTER TABLE oauth_refresh_tokens ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS rls_oauth_refresh ON oauth_refresh_tokens;
CREATE POLICY rls_oauth_refresh ON oauth_refresh_tokens
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
