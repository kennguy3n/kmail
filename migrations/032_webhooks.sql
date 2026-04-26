-- KMail — Phase 5: Tenant webhook event system.
--
-- Tenants register HTTPS endpoints to receive event callbacks for
-- email / calendar / migration lifecycle transitions. Deliveries
-- are HMAC-SHA256-signed via X-KMail-Signature using a per-
-- endpoint secret. Failed deliveries are retried with exponential
-- backoff up to 3 attempts.

BEGIN;

CREATE TABLE webhook_endpoints (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    url          TEXT NOT NULL,
    events       JSONB NOT NULL DEFAULT '[]'::jsonb,
    secret_hash  TEXT NOT NULL,
    active       BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX webhook_endpoints_tenant_idx ON webhook_endpoints (tenant_id);

CREATE TRIGGER webhook_endpoints_set_updated_at
    BEFORE UPDATE ON webhook_endpoints
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
CREATE POLICY webhook_endpoints_isolation ON webhook_endpoints
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE webhook_deliveries (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    endpoint_id    UUID NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_type     TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts       INT NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    last_status    INT NOT NULL DEFAULT 0,
    next_retry_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at   TIMESTAMPTZ
);

CREATE INDEX webhook_deliveries_pending_idx
    ON webhook_deliveries (status, next_retry_at)
    WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_idx
    ON webhook_deliveries (endpoint_id, created_at DESC);
CREATE INDEX webhook_deliveries_tenant_idx
    ON webhook_deliveries (tenant_id, created_at DESC);

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY webhook_deliveries_isolation ON webhook_deliveries
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
