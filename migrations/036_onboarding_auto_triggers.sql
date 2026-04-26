-- KMail — Phase 6: Onboarding auto-completion via webhook events.
--
-- Persistent record of onboarding steps that have been auto-
-- completed by an internal webhook event (email.received,
-- domain.verified, user.created). The onboarding service uses this
-- table to surface a "completed automatically" badge in the UI.

BEGIN;

CREATE TABLE onboarding_auto_triggers (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    step_key     TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, step_key)
);

CREATE INDEX onboarding_auto_triggers_tenant_idx
    ON onboarding_auto_triggers (tenant_id);

ALTER TABLE onboarding_auto_triggers ENABLE ROW LEVEL SECURITY;
CREATE POLICY onboarding_auto_triggers_isolation ON onboarding_auto_triggers
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
