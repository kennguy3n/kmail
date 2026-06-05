-- Rollback for 006_feature_flags.sql (WS4 Task 4 — down migration).
-- Dropping feature_flag_overrides first is redundant given the
-- ON DELETE CASCADE FK, but explicit so the rollback reads clearly
-- and does not depend on cascade ordering.
BEGIN;

DROP TABLE IF EXISTS feature_flag_overrides;
DROP TABLE IF EXISTS feature_flags;

COMMIT;
