-- ================================================================
-- 010_force_rls.sql
-- ----------------------------------------------------------------
-- Security hardening (Session 6 / SOC 2 prep): make tenant
-- row-level-security FORCED, not merely ENABLED.
--
-- Background
-- ----------
-- Every tenant-scoped table in the baseline (and later migrations)
-- runs `ALTER TABLE <t> ENABLE ROW LEVEL SECURITY` plus a
-- `<t>_tenant_isolation` policy keyed on the `app.tenant_id`
-- session GUC. However, PostgreSQL does NOT apply RLS policies to
-- a table's OWNER (or any BYPASSRLS role) unless the table is also
-- marked `FORCE ROW LEVEL SECURITY` (see the PostgreSQL docs for
-- ALTER TABLE — "row_security ... does not apply to the table
-- owner ... To apply row security to the table owner, use FORCE").
--
-- The baseline forced only two tables (`audit_log`,
-- `chat_bridge_routes`); the remaining ~60 tenant-scoped tables
-- were ENABLE-only. If the application ever connects with the role
-- that owns these tables — the default in a single-role deployment
-- where migrations and the app share one Postgres user — RLS would
-- be silently bypassed and a missing `SetTenantGUC` call would leak
-- across tenants instead of failing closed. Forcing RLS closes that
-- gap as defense-in-depth: isolation now holds regardless of which
-- (non-BYPASSRLS) role the connection uses.
--
-- A dedicated provisioning/operator role that legitimately needs to
-- read across tenants (tenant onboarding, billing reconciliation)
-- must hold the `BYPASSRLS` attribute; FORCE does not affect a
-- BYPASSRLS role, so that path is unchanged (see docs/SCHEMA.md §4).
--
-- Approach
-- --------
-- Rather than hard-code a table list (which drifts as new
-- tenant-scoped tables are added), force RLS on every table in the
-- `public` schema that already has RLS enabled but not yet forced.
-- Enabling-without-forcing is exactly the bug this migration fixes,
-- so the set "RLS enabled AND not forced" is precisely the set we
-- want to repair. Control-plane tables that intentionally allow
-- owner/operator-wide reads (`stalwart_shards`,
-- `tenant_shard_assignments`, `shard_failover_config`,
-- `feature_flags`, `feature_flag_overrides`, `signup_requests`,
-- `tenants`) never enable RLS, so they are not touched.
--
-- Idempotent: re-running is a no-op because already-forced tables
-- are filtered out by the `relforcerowsecurity = false` predicate.
-- ================================================================

BEGIN;

DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind = 'r'
          AND c.relrowsecurity = true          -- RLS already ENABLED
          AND c.relforcerowsecurity = false     -- but not yet FORCED
        ORDER BY c.relname
    LOOP
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', r.relname);
        RAISE NOTICE 'force_rls: forced ROW LEVEL SECURITY on %', r.relname;
    END LOOP;
END;
$$;

COMMIT;
