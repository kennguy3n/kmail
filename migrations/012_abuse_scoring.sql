-- KMail — Phase 3: Abuse scoring and compromised-account detection.
--
-- `abuse_alerts` captures per-signal alerts raised by the scorer
-- (volume spikes, auth-failure storms, high bounce / complaint
-- rates, unusual recipient patterns). `abuse_scores` caches the
-- current per-tenant / per-user score so dashboards can render
-- without re-computing every signal on every request.

BEGIN;

CREATE TABLE abuse_alerts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id        UUID REFERENCES users(id) ON DELETE SET NULL,
    alert_type     TEXT NOT NULL,
    severity       TEXT NOT NULL
                   CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    score          INT NOT NULL DEFAULT 0 CHECK (score >= 0),
    details        JSONB NOT NULL DEFAULT '{}'::jsonb,
    acknowledged   BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX abuse_alerts_tenant_severity_created_idx
    ON abuse_alerts (tenant_id, severity, created_at DESC);
CREATE INDEX abuse_alerts_tenant_ack_created_idx
    ON abuse_alerts (tenant_id, acknowledged, created_at DESC);

ALTER TABLE abuse_alerts ENABLE ROW LEVEL SECURITY;
CREATE POLICY abuse_alerts_tenant_isolation
    ON abuse_alerts
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE abuse_scores (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id     UUID REFERENCES users(id) ON DELETE CASCADE,
    score       INT NOT NULL DEFAULT 0 CHECK (score >= 0),
    signals     JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX abuse_scores_tenant_idx ON abuse_scores (tenant_id);

ALTER TABLE abuse_scores ENABLE ROW LEVEL SECURITY;
CREATE POLICY abuse_scores_tenant_isolation
    ON abuse_scores
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
