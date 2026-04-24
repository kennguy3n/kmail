-- KMail — Phase 3: Attachment-to-link conversion.
--
-- Large attachments (> 10-15 MB) are uploaded to zk-object-fabric
-- and delivered as presigned GET URLs with expiry / password /
-- revocation semantics. This table holds the link metadata so the
-- BFF can mint / rotate / revoke URLs without re-reading the S3
-- endpoint.

BEGIN;

CREATE TABLE attachment_links (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    object_key     TEXT NOT NULL,
    filename       TEXT NOT NULL,
    size_bytes     BIGINT NOT NULL CHECK (size_bytes >= 0),
    content_type   TEXT NOT NULL DEFAULT 'application/octet-stream',
    expiry         TIMESTAMPTZ NOT NULL,
    password_hash  TEXT NOT NULL DEFAULT '',
    revoked        BOOLEAN NOT NULL DEFAULT false,
    created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX attachment_links_tenant_created_idx
    ON attachment_links (tenant_id, created_at DESC);
CREATE INDEX attachment_links_tenant_expiry_idx
    ON attachment_links (tenant_id, expiry)
    WHERE revoked = false;

ALTER TABLE attachment_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY attachment_links_tenant_isolation ON attachment_links
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
