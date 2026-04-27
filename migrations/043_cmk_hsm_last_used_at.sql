-- KMail — Phase 8: track per-HSM "last used" timestamps.
--
-- The Phase 6 cmk_hsm_configs table only carried `last_test_at` /
-- `last_test_error`. Phase 8 wires real KMIP / PKCS#11 wire
-- traffic for envelope encryption and decryption, and operators
-- need to know when the appliance was last actually used (not
-- merely tested) so they can spot dormant wirings before they
-- silently rot.
--
-- Idempotent (uses IF NOT EXISTS) so re-running the migration on
-- partially-migrated databases is safe.

BEGIN;

ALTER TABLE cmk_hsm_configs
    ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS cmk_hsm_configs_last_used_idx
    ON cmk_hsm_configs (last_used_at DESC);

COMMIT;
