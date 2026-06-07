-- Rollback for 011_audit_seq.sql.
BEGIN;

DROP INDEX IF EXISTS audit_log_tenant_seq;
DROP INDEX IF EXISTS audit_log_seq_uniq;
ALTER TABLE audit_log ALTER COLUMN seq DROP DEFAULT;
ALTER TABLE audit_log DROP COLUMN IF EXISTS seq;
DROP SEQUENCE IF EXISTS audit_log_seq;

COMMIT;
