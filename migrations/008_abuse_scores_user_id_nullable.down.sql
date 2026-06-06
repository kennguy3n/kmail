DROP INDEX IF EXISTS abuse_scores_user_level_key;
DROP INDEX IF EXISTS abuse_scores_tenant_level_key;

-- Tenant-level rows (user_id IS NULL) cannot satisfy the restored
-- composite primary key, so drop them before re-adding it. These rows
-- are derived signals and are recomputed on the next scoring pass.
DELETE FROM abuse_scores WHERE user_id IS NULL;

ALTER TABLE abuse_scores ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE abuse_scores ADD CONSTRAINT abuse_scores_pkey PRIMARY KEY (tenant_id, user_id);
