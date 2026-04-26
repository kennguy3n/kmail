-- KMail — Phase 5: Reverse access proxy sessions.
--
-- Every approved admin proxy request opens a time-bounded
-- `admin_access_sessions` row. The proxy refuses to forward
-- traffic outside the (started_at, expires_at) window or after
-- the session has been revoked. Tied to the existing
-- approval_requests workflow so the audit trail starts from the
-- moment the admin asked for access.

BEGIN;

CREATE TABLE admin_access_sessions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    approval_request_id  UUID NOT NULL REFERENCES approval_requests(id) ON DELETE RESTRICT,
    admin_user_id        TEXT NOT NULL,
    scope                TEXT NOT NULL DEFAULT 'mailbox',
    started_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '4 hours'),
    revoked_at           TIMESTAMPTZ,
    UNIQUE (approval_request_id)
);

CREATE INDEX admin_access_sessions_tenant_idx
    ON admin_access_sessions (tenant_id, expires_at);
CREATE INDEX admin_access_sessions_approval_idx
    ON admin_access_sessions (approval_request_id);

ALTER TABLE admin_access_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY admin_access_sessions_isolation ON admin_access_sessions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
