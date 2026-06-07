-- ================================================================
-- 007_confidential_send_mls.sql
-- ----------------------------------------------------------------
-- Phase 6: wire real MLS (Messaging Layer Security) wrapping into
-- Confidential Send. Previously a confidential link only stored the
-- StrictZK blob reference and an optional password hash — the
-- per-recipient DEK was never wrapped under an MLS-derived key, so
-- "Confidential Send" was link-portal security only.
--
-- These columns let CreateSecureMessage persist the MLS material
-- needed to (a) hand the wrapping key to the recipient portal and
-- (b) re-wrap on participant changes (RekeyConfidentialMessage):
--
--   mls_wrapping_key    hex-encoded symmetric key the KChat MLS
--                       service derived for the recipient. KMail
--                       never sees the underlying MLS leaf secret.
--   mls_sender_leaf_key sender's MLS leaf identity, retained so a
--                       later rekey can re-derive without the
--                       client re-supplying it.
--   mls_participants    current participant credential set; the
--                       input to a rekey when membership changes.
--   mls_epoch           monotonically increments on every rekey so
--                       stale wrapping keys are detectable.
--
-- All columns are additive with safe defaults (empty / 0), so a
-- link created by the pre-MLS code path — or when no MLS endpoint
-- is configured — is simply a row with empty MLS material and the
-- service falls back to the link-portal flow. Idempotent: ADD
-- COLUMN IF NOT EXISTS makes a re-run a no-op. No RLS change: the
-- existing confidential_send_links_isolation policy already covers
-- the new columns.
--
-- Renumber note (006 → 007): this file was previously committed as
-- migrations/006_confidential_send_mls.sql. ws5 merged it into main as
-- the *sole* version 6 before ws4 later added
-- migrations/006_feature_flags.sql, creating a duplicate version 6 that
-- Discover() rejects. Any database that ran `migrate up` during that
-- window recorded this migration in schema_migrations under its old
-- filename. The runner keys schema_migrations by filename, so without
-- the reconciliation below the renamed file would be applied again and
-- leave an orphaned 006_confidential_send_mls.sql row. The two
-- statements rename the bookkeeping row in place (or drop a stale 006
-- row if 007 is somehow already recorded); both are no-ops on databases
-- that never applied the old 006 (fresh installs, or ones that only ran
-- 006_feature_flags). The runner's own
-- INSERT ... ON CONFLICT (filename) DO NOTHING afterwards is then a
-- no-op when the row was renamed.
-- ================================================================

BEGIN;

ALTER TABLE confidential_send_links
    ADD COLUMN IF NOT EXISTS mls_wrapping_key    TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS mls_sender_leaf_key TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS mls_participants    TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS mls_epoch           INT    NOT NULL DEFAULT 0
        CHECK (mls_epoch >= 0);

-- One-time bookkeeping reconciliation for the 006 → 007 renumber.
UPDATE schema_migrations
   SET filename = '007_confidential_send_mls.sql'
 WHERE filename = '006_confidential_send_mls.sql'
   AND NOT EXISTS (
       SELECT 1 FROM schema_migrations
        WHERE filename = '007_confidential_send_mls.sql'
   );

DELETE FROM schema_migrations
 WHERE filename = '006_confidential_send_mls.sql'
   AND EXISTS (
       SELECT 1 FROM schema_migrations
        WHERE filename = '007_confidential_send_mls.sql'
   );

COMMIT;
