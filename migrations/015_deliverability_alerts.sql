-- KMail — Phase 4: Automated deliverability alerts.
--
-- `deliverability_alerts` captures every threshold breach raised
-- by the background alert evaluator (bounce rate, complaint rate,
-- reputation drop, daily-volume spike). `alert_thresholds` lets
-- tenants override the plan defaults per metric.

BEGIN;

CREATE TABLE deliverability_alerts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    alert_type      TEXT NOT NULL,
    severity        TEXT NOT NULL
                    CHECK (severity IN ('info', 'warning', 'critical')),
    metric_name     TEXT NOT NULL,
    metric_value    DOUBLE PRECISION NOT NULL DEFAULT 0,
    threshold_value DOUBLE PRECISION NOT NULL DEFAULT 0,
    message         TEXT NOT NULL DEFAULT '',
    acknowledged    BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX deliverability_alerts_tenant_severity_created_idx
    ON deliverability_alerts (tenant_id, severity, created_at DESC);
CREATE INDEX deliverability_alerts_tenant_ack_created_idx
    ON deliverability_alerts (tenant_id, acknowledged, created_at DESC);

ALTER TABLE deliverability_alerts ENABLE ROW LEVEL SECURITY;
CREATE POLICY deliverability_alerts_tenant_isolation
    ON deliverability_alerts
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE alert_thresholds (
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    metric_name         TEXT NOT NULL,
    warning_threshold   DOUBLE PRECISION NOT NULL,
    critical_threshold  DOUBLE PRECISION NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, metric_name)
);

ALTER TABLE alert_thresholds ENABLE ROW LEVEL SECURITY;
CREATE POLICY alert_thresholds_tenant_isolation
    ON alert_thresholds
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
