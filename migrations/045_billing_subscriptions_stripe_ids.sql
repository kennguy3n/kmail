-- KMail — Phase 8: persist Stripe customer + subscription IDs.
--
-- The Lifecycle.OnTenantCreated hook (Phase 8) creates a Stripe
-- Customer + Subscription when KMAIL_STRIPE_SECRET_KEY is set. We
-- persist the resulting `cus_…` and `sub_…` identifiers on the
-- existing `billing_subscriptions` row so OnPlanChanged /
-- OnTenantDeleted can drive the corresponding Stripe API calls
-- without a hot path back to the Stripe Search API.
--
-- Both columns are nullable so tenants created before Phase 8
-- (without Stripe wiring) keep their pre-existing rows.

BEGIN;

ALTER TABLE billing_subscriptions
    ADD COLUMN IF NOT EXISTS stripe_customer_id     TEXT,
    ADD COLUMN IF NOT EXISTS stripe_subscription_id TEXT;

CREATE INDEX IF NOT EXISTS billing_subscriptions_stripe_customer_idx
    ON billing_subscriptions (stripe_customer_id) WHERE stripe_customer_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS billing_subscriptions_stripe_subscription_idx
    ON billing_subscriptions (stripe_subscription_id) WHERE stripe_subscription_id IS NOT NULL;

COMMIT;
