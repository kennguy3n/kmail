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
-- compose stack tear-downs and re-creations.
--
-- `stalwart_account_id` is the server-assigned JMAP id of the
-- `kmail-dev` principal that `scripts/stalwart-init.sh` provisions
-- (via `x:Account/set`). On a fresh registry that principal is the
-- first one created after the recovery admin, so Stalwart assigns
-- it the deterministic id `b` (the recovery admin is `d333333`).
-- In production the BFF passes this id to Stalwart as the
-- `X-KMail-Stalwart-Account-Id` header so the mail core acts as the
-- right principal. NOTE: the official `stalwartlabs/stalwart` image
-- used by the dev/CI stack does NOT implement that header-trust
-- feature, so in dev/CI the proxy authenticates as the recovery
-- admin (Basic) instead and the queried account is taken from the
-- JMAP request body (the e2e harness derives it from
-- `/jmap/session`). The value here therefore only needs to be a
-- valid, non-empty id so `resolveAccount` finds a row rather than
-- returning `accountNotFound`.
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
    'b',
    'dev-user@kmail.dev',
    'KMail Dev User',
    'owner',
    'active'
)
ON CONFLICT (tenant_id, kchat_user_id) DO NOTHING;

COMMIT;
