-- Rollback for 009_audit_chain_linearity.sql.
BEGIN;

DROP INDEX IF EXISTS audit_log_tenant_prevhash_uniq;

COMMIT;
