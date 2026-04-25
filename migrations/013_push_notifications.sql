-- KMail — Phase 3: Mobile push notifications.
--
-- Per-device web/iOS/Android push subscriptions plus a
-- notification-preferences row that the Push Service reads before
-- fanning out JMAP state changes as push messages.

BEGIN;

-- `user_id` stores either a users.id UUID or a KChat/Stalwart
-- opaque identifier. Keeping it as TEXT lets the BFF identify a
-- user by whichever claim is cheapest on the auth path without a
-- secondary lookup.
CREATE TABLE push_subscriptions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id        TEXT NOT NULL,
    device_type    TEXT NOT NULL
                   CHECK (device_type IN ('web', 'ios', 'android')),
    push_endpoint  TEXT NOT NULL,
    auth_key       TEXT NOT NULL DEFAULT '',
    p256dh_key     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, user_id, push_endpoint)
);

CREATE INDEX push_subscriptions_tenant_user_idx
    ON push_subscriptions (tenant_id, user_id);

ALTER TABLE push_subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY push_subscriptions_tenant_isolation
    ON push_subscriptions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE notification_preferences (
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id             TEXT NOT NULL,
    new_email           BOOLEAN NOT NULL DEFAULT true,
    calendar_reminder   BOOLEAN NOT NULL DEFAULT true,
    shared_inbox        BOOLEAN NOT NULL DEFAULT true,
    quiet_hours_start   TEXT NOT NULL DEFAULT '',
    quiet_hours_end     TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

ALTER TABLE notification_preferences ENABLE ROW LEVEL SECURITY;
CREATE POLICY notification_preferences_tenant_isolation
    ON notification_preferences
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
