-- KMail — Phase 5: Guided onboarding checklist.
--
-- Most checklist steps are computed live by querying existing
-- tables (domains, users, billing_events). Optional steps that
-- the admin explicitly skips are persisted here so the UI does
-- not nag indefinitely.

BEGIN;

CREATE TABLE onboarding_progress (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    step_id     TEXT NOT NULL,
    skipped_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, step_id)
);

CREATE INDEX onboarding_progress_tenant_idx ON onboarding_progress (tenant_id);

ALTER TABLE onboarding_progress ENABLE ROW LEVEL SECURITY;
CREATE POLICY onboarding_progress_isolation ON onboarding_progress
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
