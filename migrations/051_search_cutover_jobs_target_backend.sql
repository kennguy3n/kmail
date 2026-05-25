-- KMail — Phase 5 follow-up: per-transition cutover jobs.
--
-- Migration 046 created `search_cutover_jobs` with a single
-- row per tenant (PRIMARY KEY (tenant_id)). That was correct
-- when the only transition was `meilisearch -> opensearch`, but
-- once migration 050 introduced two more backends
-- (`shared_meilisearch`, `shared_opensearch`) the cutover worker
-- ships with two default transitions:
--
--   meilisearch        -> opensearch
--   shared_meilisearch -> shared_opensearch
--
-- and operators can configure more (forcing a shared tenant onto
-- a dedicated index, for example). With a tenant-keyed table, a
-- row already marked `completed` for the first transition would
-- silently block ListCandidates from picking the tenant up for
-- any subsequent transition — e.g. an operator moves a previously
-- promoted tenant back to `shared_meilisearch` and now expects
-- the worker to promote it to `shared_opensearch`, but the old
-- `completed` row hides it.
--
-- Fix: key the job row by `(tenant_id, target_backend)` so each
-- (tenant, target) pair has its own state machine. The same
-- tenant can have one completed and one pending row for
-- different targets simultaneously, and an operator-driven
-- backend revert naturally re-enables the worker because there
-- is no prior row for the new target.
--
-- Existing rows are backfilled with `target_backend = 'opensearch'`
-- — that was the only transition the worker performed before this
-- migration, so every legacy row is correct under that label. The
-- backfill is done before flipping the PK so no rows are orphaned.

BEGIN;

-- 1. Add the column NULLable first so the backfill can complete
--    without violating NOT NULL on rows that existed pre-051.
ALTER TABLE search_cutover_jobs
    ADD COLUMN IF NOT EXISTS target_backend TEXT;

-- 2. Backfill historical rows. Before this migration the only
--    transition the worker performed was meilisearch -> opensearch,
--    so every existing row implicitly targeted `opensearch`.
UPDATE search_cutover_jobs
   SET target_backend = 'opensearch'
 WHERE target_backend IS NULL;

-- 3. Lock the column down. The CHECK constraint stays loose so
--    operators can introduce custom (Source, Target) pairs via
--    `CutoverConfig.Transitions` without a schema migration —
--    the worker validates the value at runtime against its
--    registered backends.
ALTER TABLE search_cutover_jobs
    ALTER COLUMN target_backend SET NOT NULL;

-- 4. Swap the primary key. Drop the old tenant-only PK and
--    create the composite PK on (tenant_id, target_backend).
ALTER TABLE search_cutover_jobs
    DROP CONSTRAINT IF EXISTS search_cutover_jobs_pkey;

ALTER TABLE search_cutover_jobs
    ADD CONSTRAINT search_cutover_jobs_pkey
    PRIMARY KEY (tenant_id, target_backend);

-- 5. Keep the existing (state, updated_at) index — it doesn't
--    need the new column because the worker's hot path is "find
--    any row in `failed`/`in_progress` regardless of target".

COMMIT;
