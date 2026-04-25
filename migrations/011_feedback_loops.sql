-- KMail — Phase 3: Gmail Postmaster + Yahoo ARF feedback loop
-- monitoring. Persists normalized feedback events so the
-- deliverability dashboards can surface spam rate / IP reputation /
-- domain reputation / ARF complaints per tenant over time.

BEGIN;

CREATE TABLE feedback_loop_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    source      TEXT NOT NULL
                CHECK (source IN ('gmail_postmaster', 'yahoo_arf')),
    event_type  TEXT NOT NULL DEFAULT '',
    domain      TEXT NOT NULL DEFAULT '',
    data        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX feedback_loop_events_tenant_source_created_idx
    ON feedback_loop_events (tenant_id, source, created_at DESC);
CREATE INDEX feedback_loop_events_tenant_domain_created_idx
    ON feedback_loop_events (tenant_id, domain, created_at DESC);

ALTER TABLE feedback_loop_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY feedback_loop_events_tenant_isolation
    ON feedback_loop_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
