-- KMail — Phase 6: Admin proxy session expiry watcher.
--
-- The expiry worker (`internal/adminproxy/expiry_worker.go`) ticks
-- every 60s, locates rows whose `expires_at` is in the past
-- without an `expired_at` stamp or a `revoked_at`, and emits a
-- `session_expired` audit entry for each. The new column lets the
-- worker mark rows it has already processed so a single session
-- never produces duplicate audit entries.

BEGIN;

ALTER TABLE admin_access_sessions
    ADD COLUMN expired_at TIMESTAMPTZ;

CREATE INDEX admin_access_sessions_expiry_idx
    ON admin_access_sessions (expires_at)
    WHERE revoked_at IS NULL AND expired_at IS NULL;

COMMIT;
