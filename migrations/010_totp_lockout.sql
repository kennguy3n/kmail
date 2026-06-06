-- ================================================================
-- 010_totp_lockout.sql
-- ----------------------------------------------------------------
-- Security hardening (Session 6 / SOC 2 prep): per-account
-- brute-force lockout for the TOTP second factor.
--
-- TOTP codes are 6 digits and verification accepts a ±1 step
-- window, so a single guess succeeds with probability ~3e-6. The
-- /api/v1/auth/totp/check endpoint (the login-time second factor)
-- sits behind OIDC, but a first-factor compromise (e.g. a stolen
-- KChat session) would otherwise allow unlimited offline-speed
-- guessing of the second factor. Without a per-account attempt
-- ceiling, ~350k guesses (well within reach over a few hours at
-- even modest request rates) yields a >50% chance of bypass.
--
-- These two columns back a durable, per-(tenant,user) lockout in
-- internal/middleware/totp_store.go: `failed_attempts` counts
-- consecutive failures and `locked_until` parks the account until
-- a cooldown elapses. Durable (vs. in-memory / cache) so the
-- ceiling survives process restarts and is consistent across BFF
-- replicas. A successful verification clears both columns.
-- ================================================================

BEGIN;

ALTER TABLE totp_credentials
    ADD COLUMN IF NOT EXISTS failed_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS locked_until    TIMESTAMPTZ;

COMMIT;
