-- Rollback for 012_totp_lockout.sql.
BEGIN;

ALTER TABLE totp_credentials
    DROP COLUMN IF EXISTS failed_attempts,
    DROP COLUMN IF EXISTS locked_until;

COMMIT;
