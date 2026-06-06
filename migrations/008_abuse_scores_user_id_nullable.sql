-- abuse_scores.user_id is part of the primary key, which makes it
-- implicitly NOT NULL and forces a sentinel value for tenant-level
-- scores. The service writes the all-zero UUID for tenant-level rows,
-- but user_id has a foreign key to users(id); the sentinel has no
-- matching user row, so every tenant-level ScoreTenant() insert failed
-- with a foreign-key violation (SQLSTATE 23503).
--
-- Make user_id nullable (NULL == tenant-level) and enforce uniqueness
-- with partial indexes. This keeps the FK to users(id) and its
-- ON DELETE CASCADE cleanup for per-user rows, while letting
-- tenant-level rows use NULL (which the FK ignores).
ALTER TABLE abuse_scores DROP CONSTRAINT abuse_scores_pkey;
ALTER TABLE abuse_scores ALTER COLUMN user_id DROP NOT NULL;

-- At most one tenant-level (user_id IS NULL) row per tenant.
CREATE UNIQUE INDEX abuse_scores_tenant_level_key
    ON abuse_scores (tenant_id) WHERE user_id IS NULL;

-- At most one row per (tenant, user) for per-user scores.
CREATE UNIQUE INDEX abuse_scores_user_level_key
    ON abuse_scores (tenant_id, user_id) WHERE user_id IS NOT NULL;
