-- KMail — Phase 6: CardDAV Global Address List (GAL).
--
-- Per-tenant cache of the merged contact set across every account
-- in the tenant. Populated lazily by the GAL service when a
-- request comes in (or via a background refresh). Deduplicated by
-- normalized email (lower(trim(email))). Read-only from the
-- tenant user's perspective — writes happen only through the
-- per-account address books, then sync into this table.

BEGIN;

CREATE TABLE global_address_list (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email           TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',
    org             TEXT NOT NULL DEFAULT '',
    phone           TEXT NOT NULL DEFAULT '',
    source_uid      TEXT NOT NULL DEFAULT '',
    source_account  TEXT NOT NULL DEFAULT '',
    last_synced_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);

CREATE INDEX global_address_list_tenant_idx
    ON global_address_list (tenant_id);
CREATE INDEX global_address_list_search_idx
    ON global_address_list (tenant_id, lower(display_name));

ALTER TABLE global_address_list ENABLE ROW LEVEL SECURITY;
CREATE POLICY global_address_list_isolation ON global_address_list
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

COMMIT;
