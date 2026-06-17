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
-- (via `x:Account/set`). That id is NOT hard-coded here: it is passed
-- in through the `dev_stalwart_account_id` psql variable by
-- `scripts/seed-dev-tenant.sh`, which resolves it from Stalwart by the
-- principal's stable *name* (`x:Account/query`). This deliberately
-- decouples the fixture from Stalwart's id-assignment order — a
-- version bump or a change in `stalwart-init.sh` provisioning order
-- could assign the principal a different id, and a hard-coded value
-- would silently desync from the real principal (every proxied JMAP
-- call would then 404). Resolving by name removes that coupling.
--
-- In production the BFF passes this id to Stalwart as the
-- `X-KMail-Stalwart-Account-Id` header so the mail core acts as the
-- right principal. NOTE: the official `stalwartlabs/stalwart` image
-- used by the dev/CI stack does NOT implement that header-trust
-- feature, so in dev/CI the proxy authenticates as the recovery
-- admin (Basic) instead and the queried account is taken from the
-- JMAP request body (the e2e harness derives it from
-- `/jmap/session`).
--
-- Idempotent: re-runnable over an already-seeded database. The tenant
-- insert is a no-op on conflict; the user upsert refreshes
-- `stalwart_account_id` so a re-seed after a Stalwart re-provision
-- self-corrects the row to the principal's current id rather than
-- leaving a stale value behind.

\if :{?dev_stalwart_account_id}
\else
\echo '>>> seed-dev-tenant.sql requires the dev_stalwart_account_id psql variable.'
\echo '>>> Run it via scripts/seed-dev-tenant.sh (which resolves the kmail-dev'
\echo '>>> principal id from Stalwart by name), or pass -v dev_stalwart_account_id=<id>.'
DO $$ BEGIN RAISE EXCEPTION 'dev_stalwart_account_id psql variable is not set'; END $$;
\endif

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
    :'dev_stalwart_account_id',
    'dev-user@kmail.dev',
    'KMail Dev User',
    'owner',
    'active'
)
ON CONFLICT (tenant_id, kchat_user_id)
    DO UPDATE SET stalwart_account_id = EXCLUDED.stalwart_account_id;

COMMIT;
