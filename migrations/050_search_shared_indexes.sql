-- KMail — shared search indexes (multi-tenant on one index).
--
-- Phase 7 (migration 039) introduced a per-tenant search backend
-- column. The MVP shipped with one Meilisearch / OpenSearch index
-- per tenant (`kmail_<tenant>`), which is fine at hundreds of
-- tenants but does not scale to the tens-of-thousands of tenants
-- the platform now targets: a single Meilisearch node tops out
-- well before 10k discrete indexes, and OpenSearch shards become
-- a planning headache at that count.
--
-- This migration introduces three new backend values that share
-- one index per Stalwart shard, with tenant isolation enforced at
-- query time via a `tenant_id` filter:
--
--   * `shared_meilisearch`   — default for newly-provisioned tenants.
--   * `shared_opensearch`    — auto-promotion target when a tenant
--                              outgrows the Meilisearch ceiling.
--   * `dedicated_opensearch` — enterprise path; one index per
--                              tenant for hard isolation. Operator-
--                              triggered, not auto-promoted.
--
-- Existing tenants are left on whatever they currently have
-- (`meilisearch` / `opensearch`); the BFF resolves all five
-- values. New tenants default to `shared_meilisearch` via the
-- updated column default below.

BEGIN;

-- The Phase-7 check constraint was inlined into the ADD COLUMN
-- statement, which means it was emitted with an auto-generated
-- name (`tenants_search_backend_check`). DROP-by-name with IF
-- EXISTS is idempotent across re-runs of this migration.
ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_search_backend_check;

ALTER TABLE tenants
    ADD CONSTRAINT tenants_search_backend_check
        CHECK (search_backend IN (
            'meilisearch',
            'opensearch',
            'shared_meilisearch',
            'shared_opensearch',
            'dedicated_opensearch'
        ));

-- New tenants default to the shared Meilisearch index. Existing
-- rows are untouched — this only affects INSERTs that don't
-- specify `search_backend` (i.e. the normal `CreateTenant` path).
ALTER TABLE tenants
    ALTER COLUMN search_backend SET DEFAULT 'shared_meilisearch';

COMMIT;
