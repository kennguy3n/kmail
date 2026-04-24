-- KMail — Email-to-Chat Bridge routes.
--
-- Each row maps a per-tenant alias mailbox (e.g. alerts@tenant.tld)
-- to a KChat channel. The Stalwart Sieve hook / JMAP push listener
-- consults this table on inbound delivery; if a matching row
-- exists, the message is summarised and posted to the mapped
-- channel. See docs/ARCHITECTURE.md §7.

BEGIN;

CREATE TABLE chat_bridge_routes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    alias_address  TEXT NOT NULL,
    channel_id     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chat_bridge_routes_tenant_alias_uk
        UNIQUE (tenant_id, alias_address)
);

CREATE INDEX chat_bridge_routes_tenant_idx
    ON chat_bridge_routes (tenant_id);

-- RLS scoping — every query must set `app.tenant_id` first.
ALTER TABLE chat_bridge_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat_bridge_routes FORCE ROW LEVEL SECURITY;

CREATE POLICY chat_bridge_routes_tenant_isolation
    ON chat_bridge_routes
    USING (tenant_id::text = current_setting('app.tenant_id', true));

COMMIT;
