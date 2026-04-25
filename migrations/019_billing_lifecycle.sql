-- KMail — Phase 4: Tenant-level billing lifecycle.
--
-- Adds `billing_subscriptions` so the BFF can surface the current
-- subscription status (active / past_due / cancelled) and the
-- current Stripe billing-period bounds in the admin UI.
--
-- One row per tenant. The `tenants.plan` column remains the
-- authoritative plan field; this row mirrors it so the admin UI
-- can render plan + period + status without two joins.

BEGIN;

CREATE TABLE billing_subscriptions (
    tenant_id              UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    plan                   TEXT NOT NULL CHECK (plan IN ('core', 'pro', 'privacy')),
    status                 TEXT NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'past_due', 'cancelled')),
    stripe_subscription_id TEXT UNIQUE,
    current_period_start   TIMESTAMPTZ NOT NULL,
    current_period_end     TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_subscriptions_status_idx
    ON billing_subscriptions (status);

CREATE TRIGGER billing_subscriptions_set_updated_at
    BEFORE UPDATE ON billing_subscriptions
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE billing_subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_subscriptions_isolation ON billing_subscriptions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
