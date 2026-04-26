-- KMail — Phase 5: Per-resource calendar notification routing.
--
-- Phase 4 routed every calendar notification to a single
-- env-configured KChat channel (KMAIL_CALENDAR_NOTIFY_CHANNEL).
-- Phase 5 lets admins override per resource calendar (e.g. each
-- meeting room posts to a different channel). A row with
-- calendar_id = NULL is the tenant default.

BEGIN;

CREATE TABLE calendar_notification_channels (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    calendar_id  TEXT,
    channel_id   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One mapping per (tenant, calendar). The default row uses an
-- empty string for the unique index so Postgres treats every
-- "default" row as conflicting with the existing one.
CREATE UNIQUE INDEX calendar_notification_channels_unique_idx
    ON calendar_notification_channels (tenant_id, COALESCE(calendar_id, ''));

CREATE TRIGGER calendar_notification_channels_set_updated_at
    BEFORE UPDATE ON calendar_notification_channels
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE calendar_notification_channels ENABLE ROW LEVEL SECURITY;
CREATE POLICY calendar_notification_channels_isolation ON calendar_notification_channels
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
