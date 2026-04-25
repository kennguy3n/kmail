-- KMail — Phase 3: Multi-tenant Stalwart shard routing.
--
-- `stalwart_shards` is the global registry of Stalwart clusters the
-- control plane can route tenants to. It is intentionally NOT
-- RLS-scoped — shard metadata is an admin concept. Tenant ↔ shard
-- assignments live in `tenant_shard_assignments` with a unique
-- constraint on `tenant_id` so a tenant lands on exactly one shard.

BEGIN;

CREATE TABLE stalwart_shards (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT NOT NULL UNIQUE,
    stalwart_url       TEXT NOT NULL,
    postgres_dsn       TEXT NOT NULL DEFAULT '',
    max_mailboxes      INT NOT NULL DEFAULT 5000
                       CHECK (max_mailboxes >= 0),
    current_mailboxes  INT NOT NULL DEFAULT 0
                       CHECK (current_mailboxes >= 0),
    status             TEXT NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'draining', 'offline')),
    health_checked_at  TIMESTAMPTZ,
    healthy            BOOLEAN NOT NULL DEFAULT true,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER stalwart_shards_set_updated_at
    BEFORE UPDATE ON stalwart_shards
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

CREATE TABLE tenant_shard_assignments (
    tenant_id    UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    shard_id     UUID NOT NULL REFERENCES stalwart_shards(id) ON DELETE RESTRICT,
    assigned_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tenant_shard_assignments_shard_idx
    ON tenant_shard_assignments (shard_id);

-- Seed a default shard pointing at the local-dev Stalwart so boot
-- without a pre-provisioned shard still resolves to a valid URL.
INSERT INTO stalwart_shards (name, stalwart_url, postgres_dsn, max_mailboxes, status)
VALUES ('default', 'http://stalwart:8080', '', 5000, 'active')
ON CONFLICT (name) DO NOTHING;

COMMIT;
