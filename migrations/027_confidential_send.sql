-- KMail — Phase 5: Confidential Send portal.
--
-- The sender produces an encrypted blob (StrictZK envelope keyed
-- off the MLS leaf key) and stores only an opaque `blob_ref`
-- pointer here. The recipient opens the link in a public portal,
-- supplies the password (if set), and the BFF returns the
-- `blob_ref` so the React portal can decrypt client-side.

BEGIN;

CREATE TABLE confidential_send_links (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    sender_id           TEXT NOT NULL,
    link_token          TEXT NOT NULL UNIQUE,
    encrypted_blob_ref  TEXT NOT NULL,
    password_hash       TEXT NOT NULL DEFAULT '',
    expires_at          TIMESTAMPTZ NOT NULL,
    max_views           INT NOT NULL DEFAULT 1 CHECK (max_views >= 0),
    view_count          INT NOT NULL DEFAULT 0,
    revoked             BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX confidential_send_links_tenant_idx
    ON confidential_send_links (tenant_id, created_at DESC);

ALTER TABLE confidential_send_links ENABLE ROW LEVEL SECURITY;
-- Public-portal reads (`GET /api/v1/secure/{token}`) bypass RLS
-- via a query that does not set `app.tenant_id` — the lookup is
-- by the unique `link_token` only and the handler enforces token
-- + password before returning anything. Tenant-scoped admin reads
-- (list / revoke) keep using the GUC.
CREATE POLICY confidential_send_links_isolation ON confidential_send_links
    USING (
        current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    );

COMMIT;
