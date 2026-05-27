-- KMail — dev-stack default tenant seed.
--
-- The OIDC middleware's dev-bypass path synthesises an authenticated
-- identity from headers (or defaults) when the `KMAIL_DEV_BYPASS_TOKEN`
-- bearer is presented and `KMAIL_ENV=development`:
--
--     X-KMail-Dev-Tenant-Id     → 00000000-0000-0000-0000-000000000000
--     X-KMail-Dev-Kchat-User-Id → dev-user
--
-- The JMAP proxy then resolves `(tenant_id, kchat_user_id) →
-- stalwart_account_id` via Postgres (`internal/jmap/proxy.go::
-- resolveAccount`), and a missing row surfaces as
-- `urn:ietf:params:jmap:error:accountNotFound` (HTTP 404). Without
-- this seed the CI integration / nightly SDK probes would fail the
-- moment they try to drive a JMAP method through the proxy.
--
-- The dev tenant lives at a fixed UUID so it is stable across
-- compose stack tear-downs and re-creations. The `kmail-dev`
-- Stalwart account ID matches the one the `scripts/stalwart-init.sh`
-- bootstrap creates on the upstream side.
--
-- Idempotent: ON CONFLICT (id) and ON CONFLICT (tenant_id,
-- kchat_user_id) ensure re-runs over an already-seeded database are
-- no-ops. Safe to apply both on first-boot and on a re-up of the
-- compose stack.

BEGIN;

INSERT INTO tenants (id, name, slug, plan, status)
VALUES (
    '00000000-0000-0000-0000-000000000000'::uuid,
    'KMail Dev',
    'kmail-dev',
    'pro',
    'active'
)
ON CONFLICT (id) DO NOTHING;

-- Row-level security on `users` requires the `app.tenant_id` GUC
-- to match the row being inserted (see `migrations/001_baseline.sql`).
-- Set it for this transaction so the insert clears the policy check.
SELECT set_config('app.tenant_id',
                  '00000000-0000-0000-0000-000000000000',
                  true);

INSERT INTO users (
    id, tenant_id, kchat_user_id, stalwart_account_id,
    email, display_name, role, status
)
VALUES (
    '00000000-0000-0000-0000-000000000001'::uuid,
    '00000000-0000-0000-0000-000000000000'::uuid,
    'dev-user',
    'kmail-dev',
    'dev@kmail-dev',
    'KMail Dev User',
    'owner',
    'active'
)
ON CONFLICT (tenant_id, kchat_user_id) DO NOTHING;

COMMIT;
