-- KMail — Phase 3: Resource calendars and shared team calendars.
--
-- `calendar_shares` expresses the share ACL between two accounts
-- for a given calendar collection. `resource_calendars` is the
-- tenant-local registry of bookable rooms / equipment / vehicles
-- whose events are stored on a dedicated CalDAV principal.

BEGIN;

CREATE TABLE calendar_shares (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    calendar_id        TEXT NOT NULL,
    owner_account_id   TEXT NOT NULL,
    target_account_id  TEXT NOT NULL,
    permission         TEXT NOT NULL
                       CHECK (permission IN ('read', 'readwrite', 'admin')),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, calendar_id, target_account_id)
);

CREATE INDEX calendar_shares_tenant_target_idx
    ON calendar_shares (tenant_id, target_account_id);
CREATE INDEX calendar_shares_tenant_owner_idx
    ON calendar_shares (tenant_id, owner_account_id);

ALTER TABLE calendar_shares ENABLE ROW LEVEL SECURITY;
CREATE POLICY calendar_shares_tenant_isolation
    ON calendar_shares
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE resource_calendars (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    name           TEXT NOT NULL,
    resource_type  TEXT NOT NULL
                   CHECK (resource_type IN ('room', 'equipment', 'vehicle')),
    location       TEXT NOT NULL DEFAULT '',
    capacity       INT NOT NULL DEFAULT 0 CHECK (capacity >= 0),
    caldav_id      TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TRIGGER resource_calendars_set_updated_at
    BEFORE UPDATE ON resource_calendars
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

CREATE INDEX resource_calendars_tenant_idx ON resource_calendars (tenant_id);

ALTER TABLE resource_calendars ENABLE ROW LEVEL SECURITY;
CREATE POLICY resource_calendars_tenant_isolation
    ON resource_calendars
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
