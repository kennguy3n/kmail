-- ================================================================
-- 004_self_service_signup.sql
-- ----------------------------------------------------------------
-- Phase: gap closure — self-service tenant signup (Session 3).
--
-- `signup_requests` tracks a prospective tenant from the public
-- signup form through Stripe Checkout to a provisioned tenant.
-- It is intentionally PRE-TENANT: the row exists before any
-- `tenants` row, so it carries NO tenant_id and NO RLS policy.
-- Access is gated at the handler layer (public POST to create,
-- public GET-by-id for status polling) rather than by Postgres
-- row-level security.
--
-- `tenants.self_service` flags tenants that were provisioned via
-- this flow (vs. admin / sales onboarding) so billing and
-- onboarding can branch on origin.
--
-- Idempotent: guarded with IF NOT EXISTS so a re-run is a no-op.
-- Additive only — satisfies the non-destructive rule in
-- docs/SCHEMA.md §7.
-- ================================================================

BEGIN;

CREATE TABLE IF NOT EXISTS signup_requests (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email                      TEXT NOT NULL,
    org_name                   TEXT NOT NULL DEFAULT '',
    -- Mirror the tenants.plan CHECK so a signup can never request a
    -- tier the tenants table would later reject on provisioning.
    plan                       TEXT NOT NULL DEFAULT 'core'
                               CHECK (plan IN ('core', 'pro', 'privacy')),
    stripe_checkout_session_id TEXT NOT NULL DEFAULT '',
    status                     TEXT NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'active', 'failed', 'expired')),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at               TIMESTAMPTZ
);

-- Webhook completion (checkout.session.completed) looks the row up
-- by Stripe Checkout Session ID and must be idempotent: a unique
-- index on the non-empty session id guarantees a replayed webhook
-- maps to the same signup_request (and therefore the same tenant).
CREATE UNIQUE INDEX IF NOT EXISTS signup_requests_stripe_session_idx
    ON signup_requests (stripe_checkout_session_id)
    WHERE stripe_checkout_session_id <> '';

CREATE INDEX IF NOT EXISTS signup_requests_status_created_idx
    ON signup_requests (status, created_at DESC);

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS self_service BOOLEAN NOT NULL DEFAULT false;

COMMIT;
