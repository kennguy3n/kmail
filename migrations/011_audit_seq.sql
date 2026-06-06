-- ================================================================
-- 011_audit_seq.sql
-- ----------------------------------------------------------------
-- Security hardening (Session 6 / SOC 2 prep): give the audit hash
-- chain a monotonic append-order column so the tail can be found
-- deterministically.
--
-- The bug
-- -------
-- audit_log.Log() found the chain tail with
--   ORDER BY created_at DESC, id DESC LIMIT 1
-- and VerifyChain() walked the chain with
--   ORDER BY created_at ASC, id ASC.
-- Neither column reflects true append order:
--   * created_at defaults to now(), which is the *transaction
--     start* time and is constant within a transaction. Under the
--     per-tenant advisory lock added in internal/audit/audit.go a
--     transaction can BEGIN (capturing an early now()) and then
--     block on the lock, committing *after* a transaction that
--     began later — so created_at is not monotonic with commit
--     order.
--   * id is a random UUID, so the tie-break is meaningless.
-- The consequence: a later append could read a non-tail row as the
-- "latest" and attach its prev_hash to the same predecessor as an
-- existing row, forking the chain (two rows sharing one prev_hash).
-- This is exactly what the migration 009 unique index on
-- (tenant_id, prev_hash) now rejects with a 23505 — surfacing the
-- ordering bug instead of silently corrupting the chain.
--
-- The fix
-- -------
-- Add a globally monotonic `seq` backed by a sequence. Insert order
-- assigns strictly increasing values, so for any tenant the row
-- with MAX(seq) is the true tail and ORDER BY seq reproduces append
-- order exactly. Existing rows are backfilled in their current
-- (created_at, id) order — identical to the order VerifyChain used
-- before — so already-written chains keep verifying unchanged.
-- ================================================================

BEGIN;

ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS seq BIGINT;

-- Backfill existing rows preserving the historical walk order.
WITH ordered AS (
    SELECT id, row_number() OVER (ORDER BY created_at, id) AS rn
    FROM audit_log
)
UPDATE audit_log a
SET seq = o.rn
FROM ordered o
WHERE a.id = o.id AND a.seq IS NULL;

-- Sequence drives all future inserts; start past the backfilled max.
CREATE SEQUENCE IF NOT EXISTS audit_log_seq OWNED BY audit_log.seq;
SELECT setval('audit_log_seq', COALESCE((SELECT max(seq) FROM audit_log), 0) + 1, false);
ALTER TABLE audit_log ALTER COLUMN seq SET DEFAULT nextval('audit_log_seq');
ALTER TABLE audit_log ALTER COLUMN seq SET NOT NULL;

-- seq is the authoritative append order; keep it unique.
CREATE UNIQUE INDEX IF NOT EXISTS audit_log_seq_uniq ON audit_log (seq);
-- Tail lookup (Log) and ordered walk (VerifyChain) both filter by
-- tenant then order by seq; this composite index serves both.
CREATE INDEX IF NOT EXISTS audit_log_tenant_seq ON audit_log (tenant_id, seq);

COMMIT;
