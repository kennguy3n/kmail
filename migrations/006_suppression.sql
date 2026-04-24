-- KMail — Phase 3: Suppression lists + bounce tracking.
--
-- Deliverability Control Plane: hard-bounce and complaint-based
-- suppression so we don't burn tenant IP reputation by re-sending to
-- known-bad recipients. Bounce events feed the auto-suppression rule
-- (hard bounce → immediate, 3+ soft bounces / 72 h → escalate).

BEGIN;

-- ---------------------------------------------------------------
-- suppression_list
-- ---------------------------------------------------------------
--
-- One row per (tenant, email). The `reason` classifies why the
-- address was suppressed so operators can review complaints vs.
-- hard bounces vs. user-requested unsubscribes.

CREATE TABLE suppression_list (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    email       TEXT NOT NULL,
    reason      TEXT NOT NULL
                CHECK (reason IN ('hard_bounce', 'complaint',
                                   'manual', 'unsubscribe')),
    source      TEXT NOT NULL DEFAULT '',
    metadata    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);

CREATE INDEX suppression_list_tenant_email_idx
    ON suppression_list (tenant_id, email);
CREATE INDEX suppression_list_tenant_created_idx
    ON suppression_list (tenant_id, created_at DESC);

ALTER TABLE suppression_list ENABLE ROW LEVEL SECURITY;
CREATE POLICY suppression_list_tenant_isolation ON suppression_list
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------
-- bounce_events
-- ---------------------------------------------------------------
--
-- Raw bounce feed. `bounce_type` splits hard vs. soft vs. complaint
-- so the escalation rule (N soft bounces in a sliding window → add
-- to suppression_list) can query this table cheaply. `dsn_code`
-- carries the SMTP delivery-status notification code (e.g. 5.1.1).

CREATE TABLE bounce_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    email       TEXT NOT NULL,
    bounce_type TEXT NOT NULL
                CHECK (bounce_type IN ('hard', 'soft', 'complaint')),
    dsn_code    TEXT NOT NULL DEFAULT '',
    diagnostic  TEXT NOT NULL DEFAULT '',
    message_id  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bounce_events_tenant_email_created_idx
    ON bounce_events (tenant_id, email, created_at DESC);
CREATE INDEX bounce_events_tenant_created_idx
    ON bounce_events (tenant_id, created_at DESC);

ALTER TABLE bounce_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY bounce_events_tenant_isolation ON bounce_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
