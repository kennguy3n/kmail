-- KMail — Phase 7: per-tenant search backend selection.
--
-- Phase 2 shipped Meilisearch as the only search backend. Phase 7
-- adds OpenSearch as an opt-in alternative for tenants whose data
-- volume or query shape doesn't fit Meilisearch's single-node
-- model. The selection is per-tenant so a single deployment can
-- run both backends side-by-side during the rollout.

BEGIN;

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS search_backend TEXT NOT NULL DEFAULT 'meilisearch'
        CHECK (search_backend IN ('meilisearch', 'opensearch'));

CREATE INDEX IF NOT EXISTS tenants_search_backend_idx
    ON tenants (search_backend);

COMMIT;
