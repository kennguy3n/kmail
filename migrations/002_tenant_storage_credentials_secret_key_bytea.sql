-- ================================================================
-- 002_tenant_storage_credentials_secret_key_bytea.sql
-- ----------------------------------------------------------------
-- Forward-migrate dev databases that applied an earlier version of
-- `001_baseline.sql` (where `tenant_storage_credentials.encrypted_secret_key`
-- was declared `TEXT NOT NULL`) to the current shape (`BYTEA NOT NULL`).
--
-- Why this exists:
--   * PR #54 changed the column type from TEXT to BYTEA in
--     `001_baseline.sql` so the column can carry the raw output of
--     `cmk.SecretsEnvelope.Wrap` (magic prefix + nonce + ciphertext+tag,
--     which is NOT valid UTF-8 and therefore cannot survive in TEXT).
--   * `scripts/migrate.sh` keys idempotency on filename, not file
--     contents — so dev databases that already recorded
--     `001_baseline.sql` as applied will NEVER re-run it and will
--     remain on the TEXT column shape. Without this migration, the
--     new Go code's INSERT of `[]byte` ciphertext would fail with
--     a pgx type-mismatch error on those databases.
--   * Per the post-squash convention established by PR #53, any
--     schema delta after the baseline lands as an additive,
--     incremental migration (002, 003, ...) rather than an edit to
--     `001_baseline.sql`. This file is the first such delta.
--
-- Idempotent: a fresh database that just applied 001_baseline.sql
-- already has the column at BYTEA, so the conditional block is a
-- no-op. An older dev database with TEXT is upgraded in place.
-- The `USING encrypted_secret_key::bytea` cast preserves any
-- existing row data — though in practice the rows on TEXT-era
-- databases will be empty (the column carried plaintext secrets
-- and was never populated outside of dev) and the cast is just
-- defensive.
-- ================================================================

BEGIN;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM information_schema.columns
         WHERE table_schema = current_schema()
           AND table_name = 'tenant_storage_credentials'
           AND column_name = 'encrypted_secret_key'
           AND data_type = 'text'
    ) THEN
        ALTER TABLE tenant_storage_credentials
            ALTER COLUMN encrypted_secret_key
            TYPE BYTEA
            USING encrypted_secret_key::bytea;
    END IF;
END $$;

COMMIT;
