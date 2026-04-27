-- KMail — Phase 7: DKIM key rotation history.
--
-- One row per (tenant, domain, selector) tuple. The currently
-- signing key is the row with status='active'; rotated-out keys
-- stay around as 'deprecated' until their selector record drops
-- out of DNS, then move to 'revoked' for audit. The private key
-- is encrypted at rest using the existing kmail-secrets envelope
-- (the BYTEA column carries the ciphertext blob, not the raw
-- PKCS#8 PEM).

BEGIN;

CREATE TABLE IF NOT EXISTS dkim_keys (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    domain_id             UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    selector              TEXT NOT NULL,
    public_key            TEXT NOT NULL,
    private_key_encrypted BYTEA NOT NULL,
    status                TEXT NOT NULL DEFAULT 'active'
                          CHECK (status IN ('active', 'deprecated', 'revoked')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at          TIMESTAMPTZ,
    expires_at            TIMESTAMPTZ,
    revoked_at            TIMESTAMPTZ,
    UNIQUE (domain_id, selector)
);

CREATE INDEX IF NOT EXISTS dkim_keys_domain_status_idx
    ON dkim_keys (domain_id, status);
CREATE INDEX IF NOT EXISTS dkim_keys_tenant_idx
    ON dkim_keys (tenant_id);

ALTER TABLE dkim_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY dkim_keys_isolation ON dkim_keys
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- Phase 7: dunning events. One row per `invoice.payment_failed`
-- event; the dunning service counts rows inside a 30-day window
-- and suspends the tenant on the third failure.
CREATE TABLE IF NOT EXISTS billing_dunning_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    stripe_invoice_id   TEXT NOT NULL UNIQUE,
    stripe_customer_id  TEXT NOT NULL DEFAULT '',
    amount_due          BIGINT NOT NULL DEFAULT 0,
    currency            TEXT NOT NULL DEFAULT 'usd',
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS billing_dunning_events_tenant_idx
    ON billing_dunning_events (tenant_id, occurred_at DESC);

ALTER TABLE billing_dunning_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_dunning_events_isolation ON billing_dunning_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
