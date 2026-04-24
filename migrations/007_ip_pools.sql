-- KMail — Phase 3: IP Pool architecture.
--
-- Five-pool sending-IP registry per docs/ARCHITECTURE.md §8. New
-- tenants start in `new_warming` and graduate to `mature_trusted`
-- after the 30-day warmup ramp. `ip_addresses.status` tracks the
-- per-IP lifecycle; the `reputation_score` / `daily_volume` columns
-- feed the `SelectSendingIP` heuristic.

BEGIN;

-- ---------------------------------------------------------------
-- ip_pools
-- ---------------------------------------------------------------
--
-- Global registry shared across tenants. Not RLS-scoped — pool
-- metadata is an admin concept. Tenant-to-pool assignments are held
-- in `tenant_pool_assignments` below and carry RLS.

CREATE TABLE ip_pools (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    pool_type   TEXT NOT NULL
                CHECK (pool_type IN ('system_transactional',
                                      'mature_trusted',
                                      'new_warming',
                                      'restricted',
                                      'dedicated_enterprise')),
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER ip_pools_set_updated_at
    BEFORE UPDATE ON ip_pools
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

-- ---------------------------------------------------------------
-- ip_addresses
-- ---------------------------------------------------------------

CREATE TABLE ip_addresses (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pool_id          UUID NOT NULL REFERENCES ip_pools(id) ON DELETE RESTRICT,
    address          INET NOT NULL UNIQUE,
    reverse_dns      TEXT NOT NULL DEFAULT '',
    reputation_score INT NOT NULL DEFAULT 0
                     CHECK (reputation_score BETWEEN 0 AND 100),
    daily_volume     BIGINT NOT NULL DEFAULT 0 CHECK (daily_volume >= 0),
    warmup_day       INT NOT NULL DEFAULT 0 CHECK (warmup_day >= 0),
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'warming',
                                        'cooldown', 'retired')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ip_addresses_pool_idx ON ip_addresses (pool_id);
CREATE INDEX ip_addresses_pool_status_idx
    ON ip_addresses (pool_id, status, reputation_score DESC);

CREATE TRIGGER ip_addresses_set_updated_at
    BEFORE UPDATE ON ip_addresses
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

-- ---------------------------------------------------------------
-- tenant_pool_assignments
-- ---------------------------------------------------------------
--
-- Many-to-many between tenants and pools. `priority` is consulted
-- by `SelectSendingIP` to fall back from the primary pool to
-- secondary assignments when the primary has no healthy IPs.

CREATE TABLE tenant_pool_assignments (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    pool_id     UUID NOT NULL REFERENCES ip_pools(id) ON DELETE RESTRICT,
    priority    INT NOT NULL DEFAULT 100 CHECK (priority >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, pool_id)
);

CREATE INDEX tenant_pool_assignments_tenant_idx
    ON tenant_pool_assignments (tenant_id, priority);

ALTER TABLE tenant_pool_assignments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_pool_assignments_tenant_isolation
    ON tenant_pool_assignments
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- Seed the five canonical pool types so new deployments can assign
-- tenants without a manual setup step.
-- ---------------------------------------------------------------

INSERT INTO ip_pools (name, pool_type, description) VALUES
    ('system-transactional', 'system_transactional',
     'Platform notifications (password resets, DMARC reports).'),
    ('mature-trusted',       'mature_trusted',
     'Graduated tenants with clean reputation.'),
    ('new-warming',          'new_warming',
     'Default pool for new tenants during 30-day warmup ramp.'),
    ('restricted',           'restricted',
     'Reduced-volume pool for tenants under deliverability review.'),
    ('dedicated-enterprise', 'dedicated_enterprise',
     'Per-tenant dedicated IPs for enterprise add-on.')
ON CONFLICT (name) DO NOTHING;

COMMIT;
