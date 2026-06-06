-- Rollback for 008_force_rls.sql.
--
-- Reverts FORCE back to ENABLE-only (RLS stays enabled; only the
-- "policies also apply to the table owner" behaviour is dropped).
-- `audit_log` and `chat_bridge_routes` were FORCED by the baseline
-- (001), not by 008, so they are excluded here to preserve their
-- original, intentionally-stricter posture.
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
          AND c.relrowsecurity = true
          AND c.relforcerowsecurity = true
          AND c.relname NOT IN ('audit_log', 'chat_bridge_routes')
        ORDER BY c.relname
    LOOP
        EXECUTE format('ALTER TABLE public.%I NO FORCE ROW LEVEL SECURITY', r.relname);
    END LOOP;
END;
$$;

COMMIT;
