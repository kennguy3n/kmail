-- KMail — initial control-plane schema.
--
-- Owner: Go control plane (Tenant Service, DNS Onboarding,
-- Billing Service, Audit / Compliance API, Calendar Bridge).
--
-- See docs/SCHEMA.md for design rationale and indexing strategy.

BEGIN;

-- ---------------------------------------------------------------
-- Extensions
-- ---------------------------------------------------------------

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------
-- updated_at trigger
-- ---------------------------------------------------------------

CREATE OR REPLACE FUNCTION kmail_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------
-- tenants
-- ---------------------------------------------------------------

CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    plan        TEXT NOT NULL CHECK (plan IN ('core', 'pro', 'privacy')),
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'suspended', 'deleted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER tenants_set_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

-- ---------------------------------------------------------------
-- users
-- ---------------------------------------------------------------

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    email         TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'member'
                  CHECK (role IN ('owner', 'admin', 'member',
                                  'billing', 'deliverability')),
    status        TEXT NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active', 'suspended', 'deleted')),
    quota_bytes   BIGINT NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX users_tenant_id_idx ON users (tenant_id);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY users_tenant_isolation ON users
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- domains
-- ---------------------------------------------------------------

CREATE TABLE domains (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    domain         TEXT NOT NULL UNIQUE,
    verified       BOOLEAN NOT NULL DEFAULT false,
    mx_verified    BOOLEAN NOT NULL DEFAULT false,
    spf_verified   BOOLEAN NOT NULL DEFAULT false,
    dkim_verified  BOOLEAN NOT NULL DEFAULT false,
    dmarc_verified BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX domains_tenant_id_idx ON domains (tenant_id);
CREATE INDEX domains_verified_idx ON domains (tenant_id) WHERE verified = true;

CREATE TRIGGER domains_set_updated_at
    BEFORE UPDATE ON domains
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE domains ENABLE ROW LEVEL SECURITY;
CREATE POLICY domains_tenant_isolation ON domains
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- aliases
-- ---------------------------------------------------------------

CREATE TABLE aliases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    alias_email  TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX aliases_tenant_id_idx ON aliases (tenant_id);
CREATE INDEX aliases_user_id_idx ON aliases (user_id);

ALTER TABLE aliases ENABLE ROW LEVEL SECURITY;
CREATE POLICY aliases_tenant_isolation ON aliases
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- shared_inboxes
-- ---------------------------------------------------------------

CREATE TABLE shared_inboxes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    address       TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    mls_group_id  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, address)
);

CREATE INDEX shared_inboxes_tenant_id_idx ON shared_inboxes (tenant_id);

CREATE TRIGGER shared_inboxes_set_updated_at
    BEFORE UPDATE ON shared_inboxes
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE shared_inboxes ENABLE ROW LEVEL SECURITY;
CREATE POLICY shared_inboxes_tenant_isolation ON shared_inboxes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- shared_inbox_members
-- ---------------------------------------------------------------

CREATE TABLE shared_inbox_members (
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    shared_inbox_id  UUID NOT NULL REFERENCES shared_inboxes(id) ON DELETE RESTRICT,
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role             TEXT NOT NULL DEFAULT 'member'
                     CHECK (role IN ('owner', 'member', 'viewer')),
    added_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shared_inbox_id, user_id)
);

CREATE INDEX shared_inbox_members_tenant_id_idx ON shared_inbox_members (tenant_id);
CREATE INDEX shared_inbox_members_user_id_idx ON shared_inbox_members (user_id);

ALTER TABLE shared_inbox_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY shared_inbox_members_tenant_isolation ON shared_inbox_members
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- quotas
-- ---------------------------------------------------------------

CREATE TABLE quotas (
    tenant_id             UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    storage_used_bytes    BIGINT NOT NULL DEFAULT 0 CHECK (storage_used_bytes >= 0),
    storage_limit_bytes   BIGINT NOT NULL DEFAULT 0 CHECK (storage_limit_bytes >= 0),
    seat_count            INT NOT NULL DEFAULT 0 CHECK (seat_count >= 0),
    seat_limit            INT NOT NULL DEFAULT 0 CHECK (seat_limit >= 0),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER quotas_set_updated_at
    BEFORE UPDATE ON quotas
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE quotas ENABLE ROW LEVEL SECURITY;
CREATE POLICY quotas_tenant_isolation ON quotas
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- audit_log
-- ---------------------------------------------------------------

CREATE TABLE audit_log (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    actor_id       UUID,
    action         TEXT NOT NULL,
    resource_type  TEXT NOT NULL,
    resource_id    UUID,
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_created_idx
    ON audit_log (tenant_id, created_at DESC);
CREATE INDEX audit_log_tenant_resource_idx
    ON audit_log (tenant_id, resource_type, resource_id);

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_log_tenant_isolation ON audit_log
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- calendar_metadata
-- ---------------------------------------------------------------

CREATE TABLE calendar_metadata (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    owner_id       UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    calendar_type  TEXT NOT NULL
                   CHECK (calendar_type IN ('personal', 'team', 'resource')),
    name           TEXT NOT NULL,
    acl            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX calendar_metadata_tenant_id_idx ON calendar_metadata (tenant_id);
CREATE INDEX calendar_metadata_owner_id_idx ON calendar_metadata (owner_id);

CREATE TRIGGER calendar_metadata_set_updated_at
    BEFORE UPDATE ON calendar_metadata
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE calendar_metadata ENABLE ROW LEVEL SECURITY;
CREATE POLICY calendar_metadata_tenant_isolation ON calendar_metadata
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
