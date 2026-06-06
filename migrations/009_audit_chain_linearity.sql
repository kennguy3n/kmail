-- ================================================================
-- 009_audit_chain_linearity.sql
-- ----------------------------------------------------------------
-- Security hardening (Session 6 / SOC 2 prep): make the audit
-- hash-chain's linearity a database-enforced invariant.
--
-- The audit log (`audit_log`) is tamper-evident: each row links to
-- the previous via `entry_hash = SHA256(prev_hash || payload)`, and
-- `VerifyChain` walks the chain to detect edits. A *linear* chain
-- requires that, within a tenant, no two rows share the same
-- `prev_hash` — otherwise the chain forks and verification breaks.
--
-- The application now serialises appends per tenant with a
-- transaction-scoped advisory lock (see internal/audit/audit.go),
-- which prevents the read-latest-hash → insert race that could
-- otherwise fork the chain under concurrent writes. This unique
-- index is the structural backstop: even if a future code path
-- bypasses the advisory lock, the second concurrent insert sharing
-- a `prev_hash` fails loudly with a unique-violation instead of
-- silently corrupting the tamper-evidence chain.
--
-- Genesis rows carry `prev_hash = ''` (the empty string); the
-- index therefore also enforces "at most one genesis entry per
-- tenant", which is exactly the desired invariant for a single
-- linear chain per tenant.
--
-- A fresh database has an empty audit_log, so index creation is a
-- no-op build. (KMail has never shipped to production; any dev
-- database carrying pre-fix forked rows must re-run migrations from
-- a clean baseline, since a forked chain is already corrupt.)
-- ================================================================

BEGIN;

CREATE UNIQUE INDEX IF NOT EXISTS audit_log_tenant_prevhash_uniq
    ON audit_log (tenant_id, prev_hash);

COMMIT;
