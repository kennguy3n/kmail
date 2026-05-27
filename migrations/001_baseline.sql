-- KMail — squashed baseline schema.
--
-- This single migration is the final-state schema produced by
-- running the original 52 incremental migrations (001 … 052) in
-- sequence. KMail has never gone to production and the original
-- sequence contained two duplicate numeric prefixes
-- (`046_oauth_clients.sql` ↔ `046_search_cutover.sql` and
-- `051_scheduled_sends.sql` ↔ `051_search_cutover_jobs_target_backend.sql`)
-- plus a latent bug where the v1 `audit_log` table from
-- `001_initial_schema.sql` was unconditionally re-created by
-- `004_audit_log.sql` with the chained-hash tamper-evidence shape.
-- Squashing eliminates both classes of breakage.
--
-- Owner: Go control plane (Tenant Service, DNS Onboarding,
-- Billing Service, Audit / Compliance API, Calendar Bridge,
-- Migration Orchestrator, Webhooks, OAuth2, Search cutover,
-- Scheduled Send / Snooze workers).
--
-- See `docs/SCHEMA.md` for design rationale and indexing strategy,
-- and `docs/ARCHITECTURE.md` §7 for the service topology.
--
-- Ordering invariants this file relies on:
--   1. Functions before triggers that call them.
--   2. Parent tables before child tables (FK direction).
--   3. Indexes / triggers / RLS policies live next to their tables.
--   4. Cross-table FKs that can be inline (no cycles) are inline;
--      the only declarative dependency we explicitly resolve is
--      `oauth_access_tokens.refresh_token_id` → `oauth_refresh_tokens`,
--      which is created in dependency order so the FK is inline.
--
-- A single BEGIN/COMMIT wraps everything so a partial failure
-- leaves an empty schema instead of a half-applied one.

BEGIN;

-- ================================================================
-- Extensions
-- ================================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ================================================================
-- Shared trigger function: updated_at auto-touch
-- ================================================================

CREATE OR REPLACE FUNCTION kmail_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ================================================================
-- Core tenancy: tenants, users, domains, aliases
-- ================================================================

CREATE TABLE tenants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    slug            TEXT NOT NULL UNIQUE,
    plan            TEXT NOT NULL CHECK (plan IN ('core', 'pro', 'privacy')),
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended', 'deleted')),
    -- search_backend: the per-tenant index backend (Phase 7 + the
    -- multi-tenant shared / dedicated extensions added later). New
    -- tenants land on `shared_meilisearch` by default; auto-promotion
    -- to `shared_opensearch` is driven by the search cutover worker.
    search_backend  TEXT NOT NULL DEFAULT 'shared_meilisearch'
                    CHECK (search_backend IN (
                        'meilisearch',
                        'opensearch',
                        'shared_meilisearch',
                        'shared_opensearch',
                        'dedicated_opensearch'
                    )),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tenants_search_backend_idx ON tenants (search_backend);

CREATE TRIGGER tenants_set_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

CREATE TABLE users (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    kchat_user_id        TEXT NOT NULL,
    stalwart_account_id  TEXT NOT NULL,
    email                TEXT NOT NULL UNIQUE,
    display_name         TEXT NOT NULL,
    role                 TEXT NOT NULL DEFAULT 'member'
                         CHECK (role IN ('owner', 'admin', 'member',
                                         'billing', 'deliverability')),
    status               TEXT NOT NULL DEFAULT 'active'
                         CHECK (status IN ('active', 'suspended', 'deleted')),
    quota_bytes          BIGINT NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
    -- account_type lets the billing service exclude shared-inbox /
    -- service accounts from seat counts without scanning address
    -- patterns. Defaults to `user` so callers that don't set it
    -- behave like the Phase 1 single-class model.
    account_type         TEXT NOT NULL DEFAULT 'user'
                         CHECK (account_type IN ('user', 'shared_inbox', 'service')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, kchat_user_id),
    UNIQUE (stalwart_account_id)
);

CREATE INDEX users_tenant_id_idx ON users (tenant_id);
CREATE INDEX users_tenant_account_type_idx
    ON users (tenant_id, account_type) WHERE status = 'active';

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
CREATE POLICY users_tenant_isolation ON users
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE domains (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    domain         TEXT NOT NULL UNIQUE,
    verified       BOOLEAN NOT NULL DEFAULT false,
    mx_verified    BOOLEAN NOT NULL DEFAULT false,
    spf_verified   BOOLEAN NOT NULL DEFAULT false,
    dkim_verified  BOOLEAN NOT NULL DEFAULT false,
    dmarc_verified BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX domains_tenant_id_idx ON domains (tenant_id);
CREATE INDEX domains_verified_idx ON domains (tenant_id) WHERE verified = true;

CREATE TRIGGER domains_set_updated_at
    BEFORE UPDATE ON domains
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE domains ENABLE ROW LEVEL SECURITY;
CREATE POLICY domains_tenant_isolation ON domains
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE aliases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    alias_email  TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX aliases_tenant_id_idx ON aliases (tenant_id);
CREATE INDEX aliases_user_id_idx ON aliases (user_id);

CREATE TRIGGER aliases_set_updated_at
    BEFORE UPDATE ON aliases
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE aliases ENABLE ROW LEVEL SECURITY;
CREATE POLICY aliases_tenant_isolation ON aliases
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Shared inboxes (Phase 1 base + Phase 4 workflow layer)
-- ================================================================

CREATE TABLE shared_inboxes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    address       TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    mls_group_id  TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, address)
);

CREATE INDEX shared_inboxes_tenant_id_idx ON shared_inboxes (tenant_id);

CREATE TRIGGER shared_inboxes_set_updated_at
    BEFORE UPDATE ON shared_inboxes
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE shared_inboxes ENABLE ROW LEVEL SECURITY;
CREATE POLICY shared_inboxes_tenant_isolation ON shared_inboxes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE shared_inbox_members (
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    shared_inbox_id  UUID NOT NULL REFERENCES shared_inboxes(id) ON DELETE RESTRICT,
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role             TEXT NOT NULL DEFAULT 'member'
                     CHECK (role IN ('owner', 'member', 'viewer')),
    added_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shared_inbox_id, user_id)
);

CREATE INDEX shared_inbox_members_tenant_id_idx ON shared_inbox_members (tenant_id);
CREATE INDEX shared_inbox_members_user_id_idx ON shared_inbox_members (user_id);

ALTER TABLE shared_inbox_members ENABLE ROW LEVEL SECURITY;
CREATE POLICY shared_inbox_members_tenant_isolation ON shared_inbox_members
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE shared_inbox_assignments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    shared_inbox_id   UUID NOT NULL REFERENCES shared_inboxes(id) ON DELETE RESTRICT,
    email_id          TEXT NOT NULL,
    assignee_user_id  UUID REFERENCES users(id) ON DELETE SET NULL,
    status            TEXT NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open', 'in_progress',
                                         'waiting', 'resolved', 'closed')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, shared_inbox_id, email_id)
);

CREATE INDEX shared_inbox_assignments_tenant_inbox_status_idx
    ON shared_inbox_assignments (tenant_id, shared_inbox_id, status);
CREATE INDEX shared_inbox_assignments_tenant_inbox_email_idx
    ON shared_inbox_assignments (tenant_id, shared_inbox_id, email_id);
CREATE INDEX shared_inbox_assignments_tenant_assignee_idx
    ON shared_inbox_assignments (tenant_id, assignee_user_id, status);

CREATE TRIGGER shared_inbox_assignments_set_updated_at
    BEFORE UPDATE ON shared_inbox_assignments
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE shared_inbox_assignments ENABLE ROW LEVEL SECURITY;
CREATE POLICY shared_inbox_assignments_tenant_isolation
    ON shared_inbox_assignments
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE shared_inbox_notes (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    shared_inbox_id   UUID NOT NULL REFERENCES shared_inboxes(id) ON DELETE RESTRICT,
    email_id          TEXT NOT NULL,
    author_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    note_text         TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX shared_inbox_notes_tenant_inbox_email_idx
    ON shared_inbox_notes (tenant_id, shared_inbox_id, email_id, created_at DESC);

ALTER TABLE shared_inbox_notes ENABLE ROW LEVEL SECURITY;
CREATE POLICY shared_inbox_notes_tenant_isolation
    ON shared_inbox_notes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Quotas
-- ================================================================

CREATE TABLE quotas (
    tenant_id             UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    storage_used_bytes    BIGINT NOT NULL DEFAULT 0 CHECK (storage_used_bytes >= 0),
    storage_limit_bytes   BIGINT NOT NULL DEFAULT 0 CHECK (storage_limit_bytes >= 0),
    seat_count            INT NOT NULL DEFAULT 0 CHECK (seat_count >= 0),
    seat_limit            INT NOT NULL DEFAULT 0 CHECK (seat_limit >= 0),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER quotas_set_updated_at
    BEFORE UPDATE ON quotas
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE quotas ENABLE ROW LEVEL SECURITY;
CREATE POLICY quotas_tenant_isolation ON quotas
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Audit log (tamper-evident, chained hash)
-- ----------------------------------------------------------------
-- This is the v2 shape from the legacy `004_audit_log.sql` (chained
-- `prev_hash` / `entry_hash`, `actor_type`, `ip_address`,
-- `user_agent`). The earlier v1 shape from the original Phase 1
-- migration was superseded before any production deploy. RLS is
-- FORCEd so even table-owner sessions cannot read across tenants.
-- ================================================================

CREATE TABLE audit_log (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_id       TEXT NOT NULL,
    actor_type     TEXT NOT NULL CHECK (actor_type IN ('user', 'system', 'admin')),
    action         TEXT NOT NULL,
    resource_type  TEXT NOT NULL,
    resource_id    TEXT,
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip_address     INET,
    user_agent     TEXT,
    prev_hash      TEXT NOT NULL DEFAULT '',
    entry_hash     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_tenant_time_idx
    ON audit_log (tenant_id, created_at DESC);

CREATE INDEX audit_log_action_idx
    ON audit_log (tenant_id, action, created_at DESC);

ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

CREATE POLICY audit_log_tenant_isolation
    ON audit_log
    USING (tenant_id::text = current_setting('app.tenant_id', true));

-- ================================================================
-- Calendar metadata + per-resource notification routing
-- ================================================================

CREATE TABLE calendar_metadata (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    owner_id       UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    calendar_type  TEXT NOT NULL
                   CHECK (calendar_type IN ('personal', 'team', 'resource')),
    name           TEXT NOT NULL,
    acl            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX calendar_metadata_tenant_id_idx ON calendar_metadata (tenant_id);
CREATE INDEX calendar_metadata_owner_id_idx ON calendar_metadata (owner_id);

CREATE TRIGGER calendar_metadata_set_updated_at
    BEFORE UPDATE ON calendar_metadata
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE calendar_metadata ENABLE ROW LEVEL SECURITY;
CREATE POLICY calendar_metadata_tenant_isolation ON calendar_metadata
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

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

-- ================================================================
-- Migration Orchestrator: IMAP/Gmail import job queue
-- ----------------------------------------------------------------
-- Mirrored by `internal/migration.MigrationJob`. Workers update
-- `status`, `progress_pct`, `messages_synced`, `started_at`, and
-- `completed_at` as the sync advances.
-- ================================================================

CREATE TABLE migration_jobs (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    source_host                TEXT NOT NULL,
    source_user                TEXT NOT NULL,
    source_password_encrypted  TEXT,
    dest_user                  TEXT NOT NULL,
    status                     TEXT NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'running',
                                                 'paused', 'cancelled',
                                                 'failed', 'completed')),
    progress_pct               INT NOT NULL DEFAULT 0
                               CHECK (progress_pct BETWEEN 0 AND 100),
    messages_total             INT,
    messages_synced            INT,
    started_at                 TIMESTAMPTZ,
    completed_at               TIMESTAMPTZ,
    error_msg                  TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX migration_jobs_tenant_id_idx ON migration_jobs (tenant_id);
CREATE INDEX migration_jobs_status_idx
    ON migration_jobs (status)
    WHERE status IN ('pending', 'running');

CREATE TRIGGER migration_jobs_set_updated_at
    BEFORE UPDATE ON migration_jobs
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE migration_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY migration_jobs_tenant ON migration_jobs
    USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- ================================================================
-- Email-to-Chat bridge routes
-- ================================================================

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

ALTER TABLE chat_bridge_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE chat_bridge_routes FORCE ROW LEVEL SECURITY;

CREATE POLICY chat_bridge_routes_tenant_isolation
    ON chat_bridge_routes
    USING (tenant_id::text = current_setting('app.tenant_id', true));

-- ================================================================
-- Billing: events, subscriptions, dunning
-- ================================================================

CREATE TABLE billing_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    event_type     TEXT NOT NULL
                   CHECK (event_type IN (
                       'seat_added', 'seat_removed',
                       'storage_delta', 'storage_snapshot',
                       'plan_changed', 'invoice_generated',
                       'limit_adjusted'
                   )),
    seat_count     INT,
    storage_delta  BIGINT,
    amount_cents   BIGINT,
    metadata       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_events_tenant_created_idx
    ON billing_events (tenant_id, created_at DESC);
CREATE INDEX billing_events_tenant_type_created_idx
    ON billing_events (tenant_id, event_type, created_at DESC);

ALTER TABLE billing_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_events_tenant_isolation ON billing_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- billing_subscriptions: one row per tenant mirrors `tenants.plan`
-- with subscription status + Stripe billing-period bounds, plus the
-- Stripe customer + subscription identifiers persisted so plan-
-- change / cancel handlers can reach the Stripe API without a hot
-- path through the Search API. Phase 8 introduced the Stripe IDs
-- and they remain nullable so tenants created before Stripe wiring
-- (or tenants without a live Stripe customer) keep working.
CREATE TABLE billing_subscriptions (
    tenant_id              UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    plan                   TEXT NOT NULL CHECK (plan IN ('core', 'pro', 'privacy')),
    status                 TEXT NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'past_due', 'cancelled')),
    stripe_customer_id     TEXT,
    stripe_subscription_id TEXT UNIQUE,
    current_period_start   TIMESTAMPTZ NOT NULL,
    current_period_end     TIMESTAMPTZ NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_subscriptions_status_idx
    ON billing_subscriptions (status);
CREATE INDEX billing_subscriptions_stripe_customer_idx
    ON billing_subscriptions (stripe_customer_id) WHERE stripe_customer_id IS NOT NULL;
CREATE INDEX billing_subscriptions_stripe_subscription_idx
    ON billing_subscriptions (stripe_subscription_id) WHERE stripe_subscription_id IS NOT NULL;

CREATE TRIGGER billing_subscriptions_set_updated_at
    BEFORE UPDATE ON billing_subscriptions
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE billing_subscriptions ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_subscriptions_isolation ON billing_subscriptions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- billing_dunning_events: one row per `invoice.payment_failed`
-- event; the dunning service counts rows in a 30-day window and
-- suspends the tenant on the third failure.
CREATE TABLE billing_dunning_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    stripe_invoice_id   TEXT NOT NULL UNIQUE,
    stripe_customer_id  TEXT NOT NULL DEFAULT '',
    amount_due          BIGINT NOT NULL DEFAULT 0,
    currency            TEXT NOT NULL DEFAULT 'usd',
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX billing_dunning_events_tenant_idx
    ON billing_dunning_events (tenant_id, occurred_at DESC);

ALTER TABLE billing_dunning_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY billing_dunning_events_isolation ON billing_dunning_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Deliverability: suppression, bounce, FBLs, DMARC, IP pools, alerts
-- ================================================================

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

-- ip_pools: global five-pool registry (NOT RLS-scoped — admin
-- concept). Tenant-to-pool assignments carry RLS via
-- `tenant_pool_assignments` below.
CREATE TABLE ip_pools (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    pool_type   TEXT NOT NULL
                CHECK (pool_type IN ('system_transactional',
                                      'mature_trusted',
                                      'new_warming',
                                      'restricted',
                                      'dedicated_enterprise')),
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER ip_pools_set_updated_at
    BEFORE UPDATE ON ip_pools
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

CREATE TABLE ip_addresses (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pool_id          UUID NOT NULL REFERENCES ip_pools(id) ON DELETE RESTRICT,
    address          INET NOT NULL UNIQUE,
    reverse_dns      TEXT NOT NULL DEFAULT '',
    reputation_score INT NOT NULL DEFAULT 0
                     CHECK (reputation_score BETWEEN 0 AND 100),
    daily_volume     BIGINT NOT NULL DEFAULT 0 CHECK (daily_volume >= 0),
    warmup_day       INT NOT NULL DEFAULT 0 CHECK (warmup_day >= 0),
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'warming',
                                        'cooldown', 'retired')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ip_addresses_pool_idx ON ip_addresses (pool_id);
CREATE INDEX ip_addresses_pool_status_idx
    ON ip_addresses (pool_id, status, reputation_score DESC);

CREATE TRIGGER ip_addresses_set_updated_at
    BEFORE UPDATE ON ip_addresses
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

CREATE TABLE tenant_pool_assignments (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    pool_id     UUID NOT NULL REFERENCES ip_pools(id) ON DELETE RESTRICT,
    priority    INT NOT NULL DEFAULT 100 CHECK (priority >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, pool_id)
);

CREATE INDEX tenant_pool_assignments_tenant_idx
    ON tenant_pool_assignments (tenant_id, priority);

ALTER TABLE tenant_pool_assignments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_pool_assignments_tenant_isolation
    ON tenant_pool_assignments
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE dmarc_reports (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    domain_id         UUID REFERENCES domains(id) ON DELETE SET NULL,
    report_id         TEXT NOT NULL DEFAULT '',
    org_name          TEXT NOT NULL DEFAULT '',
    email             TEXT NOT NULL DEFAULT '',
    date_range_begin  TIMESTAMPTZ NOT NULL,
    date_range_end    TIMESTAMPTZ NOT NULL,
    domain            TEXT NOT NULL,
    adkim             TEXT NOT NULL DEFAULT '',
    aspf              TEXT NOT NULL DEFAULT '',
    policy            TEXT NOT NULL DEFAULT '',
    pass_count        BIGINT NOT NULL DEFAULT 0,
    fail_count        BIGINT NOT NULL DEFAULT 0,
    records           JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw_xml           TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX dmarc_reports_tenant_domain_begin_idx
    ON dmarc_reports (tenant_id, domain_id, date_range_begin DESC);
CREATE INDEX dmarc_reports_tenant_begin_idx
    ON dmarc_reports (tenant_id, date_range_begin DESC);

ALTER TABLE dmarc_reports ENABLE ROW LEVEL SECURITY;
CREATE POLICY dmarc_reports_tenant_isolation ON dmarc_reports
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE attachment_links (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    object_key     TEXT NOT NULL,
    filename       TEXT NOT NULL,
    size_bytes     BIGINT NOT NULL CHECK (size_bytes >= 0),
    content_type   TEXT NOT NULL DEFAULT 'application/octet-stream',
    expiry         TIMESTAMPTZ NOT NULL,
    password_hash  TEXT NOT NULL DEFAULT '',
    revoked        BOOLEAN NOT NULL DEFAULT false,
    created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX attachment_links_tenant_created_idx
    ON attachment_links (tenant_id, created_at DESC);
CREATE INDEX attachment_links_tenant_expiry_idx
    ON attachment_links (tenant_id, expiry)
    WHERE revoked = false;

ALTER TABLE attachment_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY attachment_links_tenant_isolation ON attachment_links
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE tenant_send_limits (
    tenant_id        UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    daily_limit      INT NOT NULL CHECK (daily_limit >= 0),
    hourly_limit     INT NOT NULL CHECK (hourly_limit >= 0),
    warmup_override  INT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER tenant_send_limits_set_updated_at
    BEFORE UPDATE ON tenant_send_limits
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE tenant_send_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_send_limits_tenant_isolation
    ON tenant_send_limits
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE feedback_loop_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    source      TEXT NOT NULL
                CHECK (source IN ('gmail_postmaster', 'yahoo_arf')),
    event_type  TEXT NOT NULL DEFAULT '',
    domain      TEXT NOT NULL DEFAULT '',
    data        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX feedback_loop_events_tenant_source_created_idx
    ON feedback_loop_events (tenant_id, source, created_at DESC);
CREATE INDEX feedback_loop_events_tenant_domain_created_idx
    ON feedback_loop_events (tenant_id, domain, created_at DESC);

ALTER TABLE feedback_loop_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY feedback_loop_events_tenant_isolation
    ON feedback_loop_events
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

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

-- ================================================================
-- Push notifications
-- ================================================================

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

-- ================================================================
-- Stalwart shard registry
-- ================================================================

CREATE TABLE stalwart_shards (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name               TEXT NOT NULL UNIQUE,
    stalwart_url       TEXT NOT NULL,
    postgres_dsn       TEXT NOT NULL DEFAULT '',
    max_mailboxes      INT NOT NULL DEFAULT 5000
                       CHECK (max_mailboxes >= 0),
    current_mailboxes  INT NOT NULL DEFAULT 0
                       CHECK (current_mailboxes >= 0),
    status             TEXT NOT NULL DEFAULT 'active'
                       CHECK (status IN ('active', 'draining', 'offline')),
    health_checked_at  TIMESTAMPTZ,
    healthy            BOOLEAN NOT NULL DEFAULT true,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER stalwart_shards_set_updated_at
    BEFORE UPDATE ON stalwart_shards
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

CREATE TABLE tenant_shard_assignments (
    tenant_id    UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    shard_id     UUID NOT NULL REFERENCES stalwart_shards(id) ON DELETE RESTRICT,
    assigned_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tenant_shard_assignments_shard_idx
    ON tenant_shard_assignments (shard_id);

CREATE TABLE shard_failover_config (
    shard_id        UUID NOT NULL REFERENCES stalwart_shards(id) ON DELETE CASCADE,
    backup_shard_id UUID NOT NULL REFERENCES stalwart_shards(id) ON DELETE CASCADE,
    priority        INT  NOT NULL DEFAULT 100,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (shard_id, backup_shard_id),
    CHECK (shard_id <> backup_shard_id)
);

CREATE INDEX shard_failover_config_priority_idx
    ON shard_failover_config (shard_id, priority);

-- ================================================================
-- Per-tenant storage credentials (zk-object-fabric)
-- ================================================================

CREATE TABLE tenant_storage_credentials (
    tenant_id              UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE RESTRICT,
    bucket_name            TEXT NOT NULL UNIQUE,
    access_key             TEXT NOT NULL,
    encrypted_secret_key   TEXT NOT NULL,
    placement_policy_ref   TEXT NOT NULL DEFAULT '',
    encryption_mode_default TEXT NOT NULL DEFAULT 'managed'
                           CHECK (encryption_mode_default IN ('managed', 'client_side', 'public_distribution')),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER tenant_storage_credentials_set_updated_at
    BEFORE UPDATE ON tenant_storage_credentials
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE tenant_storage_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_storage_credentials_isolation ON tenant_storage_credentials
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Retention, exports, approvals
-- ================================================================

CREATE TABLE retention_policies (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    policy_type     TEXT NOT NULL CHECK (policy_type IN ('archive', 'delete')),
    retention_days  INT  NOT NULL CHECK (retention_days > 0),
    applies_to      TEXT NOT NULL DEFAULT 'all' CHECK (applies_to IN ('all', 'mailbox', 'label')),
    target_ref      TEXT NOT NULL DEFAULT '',
    enabled         BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX retention_policies_tenant_idx
    ON retention_policies (tenant_id);

CREATE TRIGGER retention_policies_set_updated_at
    BEFORE UPDATE ON retention_policies
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE retention_policies ENABLE ROW LEVEL SECURITY;
CREATE POLICY retention_policies_isolation ON retention_policies
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE retention_enforcement_log (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    policy_id         UUID NOT NULL REFERENCES retention_policies(id) ON DELETE CASCADE,
    emails_processed  INT NOT NULL DEFAULT 0,
    emails_deleted    INT NOT NULL DEFAULT 0,
    emails_archived   INT NOT NULL DEFAULT 0,
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    error             TEXT NOT NULL DEFAULT '',
    notes             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX retention_enforcement_tenant_idx
    ON retention_enforcement_log (tenant_id, started_at DESC);
CREATE INDEX retention_enforcement_policy_idx
    ON retention_enforcement_log (policy_id);

ALTER TABLE retention_enforcement_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY retention_enforcement_isolation ON retention_enforcement_log
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE approval_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    requester_id    TEXT NOT NULL,
    action          TEXT NOT NULL,
    target_resource TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    approver_id     TEXT,
    reason          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '7 days')
);

CREATE INDEX approval_requests_tenant_status_idx
    ON approval_requests (tenant_id, status);

ALTER TABLE approval_requests ENABLE ROW LEVEL SECURITY;
CREATE POLICY approval_requests_isolation ON approval_requests
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE approval_config (
    tenant_id         UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    action            TEXT NOT NULL,
    requires_approval BOOLEAN NOT NULL DEFAULT false,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, action)
);

CREATE TRIGGER approval_config_set_updated_at
    BEFORE UPDATE ON approval_config
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE approval_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY approval_config_isolation ON approval_config
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE export_jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    requester_id    TEXT NOT NULL,
    format          TEXT NOT NULL CHECK (format IN ('mbox', 'eml', 'pst_stub')),
    scope           TEXT NOT NULL DEFAULT 'all'
                    CHECK (scope IN ('all', 'mailbox', 'date_range')),
    scope_ref       TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    download_url    TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ
);

CREATE INDEX export_jobs_tenant_status_idx
    ON export_jobs (tenant_id, status, created_at DESC);

ALTER TABLE export_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY export_jobs_isolation ON export_jobs
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Privacy modes: vault folders, CMK, protected folders, Confidential
-- Send portal
-- ================================================================

CREATE TABLE vault_folders (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL,
    folder_name     TEXT NOT NULL,
    encryption_mode TEXT NOT NULL DEFAULT 'StrictZK'
                    CHECK (encryption_mode IN ('StrictZK')),
    wrapped_dek     BYTEA NOT NULL DEFAULT ''::bytea,
    key_algorithm   TEXT NOT NULL DEFAULT 'XChaCha20-Poly1305',
    nonce           BYTEA NOT NULL DEFAULT ''::bytea,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX vault_folders_tenant_user_idx
    ON vault_folders (tenant_id, user_id, folder_name);

CREATE TRIGGER vault_folders_set_updated_at
    BEFORE UPDATE ON vault_folders
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE vault_folders ENABLE ROW LEVEL SECURITY;
CREATE POLICY vault_folders_isolation ON vault_folders
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE customer_managed_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_fingerprint TEXT NOT NULL UNIQUE,
    public_key_pem  TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'deprecated', 'revoked')),
    algorithm       TEXT NOT NULL DEFAULT 'RSA-OAEP-256',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX customer_managed_keys_tenant_idx
    ON customer_managed_keys (tenant_id, status);

CREATE TRIGGER customer_managed_keys_set_updated_at
    BEFORE UPDATE ON customer_managed_keys
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE customer_managed_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY customer_managed_keys_isolation ON customer_managed_keys
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- cmk_hsm_configs: BYOC HSM (KMIP or PKCS#11) used to wrap CMK
-- material. `last_test_at` records the most recent connectivity
-- check; `last_used_at` records the most recent envelope encrypt
-- or decrypt so operators can spot dormant wirings before they
-- silently rot.
CREATE TABLE cmk_hsm_configs (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider_type         TEXT NOT NULL CHECK (provider_type IN ('kmip', 'pkcs11')),
    endpoint              TEXT NOT NULL,
    slot_id               TEXT NOT NULL DEFAULT '',
    credentials_encrypted BYTEA NOT NULL,
    status                TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'active', 'failed', 'revoked')),
    last_test_at          TIMESTAMPTZ,
    last_test_error       TEXT NOT NULL DEFAULT '',
    last_used_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX cmk_hsm_configs_tenant_idx ON cmk_hsm_configs (tenant_id);
CREATE INDEX cmk_hsm_configs_last_used_idx
    ON cmk_hsm_configs (last_used_at DESC);

CREATE TRIGGER cmk_hsm_configs_set_updated_at
    BEFORE UPDATE ON cmk_hsm_configs
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE cmk_hsm_configs ENABLE ROW LEVEL SECURITY;
CREATE POLICY cmk_hsm_configs_isolation ON cmk_hsm_configs
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE protected_folders (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    owner_id        TEXT NOT NULL,
    folder_name     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX protected_folders_tenant_owner_idx
    ON protected_folders (tenant_id, owner_id);

CREATE TRIGGER protected_folders_set_updated_at
    BEFORE UPDATE ON protected_folders
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE protected_folders ENABLE ROW LEVEL SECURITY;
CREATE POLICY protected_folders_isolation ON protected_folders
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE protected_folder_access (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    folder_id       UUID NOT NULL REFERENCES protected_folders(id) ON DELETE CASCADE,
    grantee_id      TEXT NOT NULL,
    permission      TEXT NOT NULL DEFAULT 'read'
                    CHECK (permission IN ('read', 'read_write')),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (folder_id, grantee_id)
);

CREATE INDEX protected_folder_access_tenant_idx
    ON protected_folder_access (tenant_id, folder_id);

ALTER TABLE protected_folder_access ENABLE ROW LEVEL SECURITY;
CREATE POLICY protected_folder_access_isolation ON protected_folder_access
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE protected_folder_access_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    folder_id       UUID NOT NULL REFERENCES protected_folders(id) ON DELETE CASCADE,
    actor_id        TEXT NOT NULL,
    action          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX protected_folder_access_log_idx
    ON protected_folder_access_log (tenant_id, folder_id, created_at DESC);

ALTER TABLE protected_folder_access_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY protected_folder_access_log_isolation ON protected_folder_access_log
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- confidential_send_links: portal-style external delivery of an
-- encrypted blob. Public-portal reads (`GET /api/v1/secure/{token}`)
-- bypass RLS via a query that does not set `app.tenant_id` — the
-- lookup is by the unique `link_token` only and the handler enforces
-- token + password before returning anything. Tenant-scoped admin
-- reads (list / revoke) keep using the GUC.
CREATE TABLE confidential_send_links (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    sender_id           TEXT NOT NULL,
    link_token          TEXT NOT NULL UNIQUE,
    encrypted_blob_ref  TEXT NOT NULL,
    password_hash       TEXT NOT NULL DEFAULT '',
    expires_at          TIMESTAMPTZ NOT NULL,
    max_views           INT NOT NULL DEFAULT 1 CHECK (max_views >= 0),
    view_count          INT NOT NULL DEFAULT 0,
    revoked             BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX confidential_send_links_tenant_idx
    ON confidential_send_links (tenant_id, created_at DESC);

ALTER TABLE confidential_send_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY confidential_send_links_isolation ON confidential_send_links
    USING (
        current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    );

-- ================================================================
-- SCIM tokens, admin access sessions
-- ================================================================

CREATE TABLE scim_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX scim_tokens_tenant_idx ON scim_tokens (tenant_id);
CREATE INDEX scim_tokens_active_idx ON scim_tokens (token_hash) WHERE revoked_at IS NULL;

ALTER TABLE scim_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY scim_tokens_isolation ON scim_tokens
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- admin_access_sessions: reverse-proxy session windows. `expired_at`
-- is stamped by the expiry worker (`internal/adminproxy/
-- expiry_worker.go`) so a single session never produces duplicate
-- `session_expired` audit entries.
CREATE TABLE admin_access_sessions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    approval_request_id  UUID NOT NULL REFERENCES approval_requests(id) ON DELETE RESTRICT,
    admin_user_id        TEXT NOT NULL,
    scope                TEXT NOT NULL DEFAULT 'mailbox',
    started_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '4 hours'),
    revoked_at           TIMESTAMPTZ,
    expired_at           TIMESTAMPTZ,
    UNIQUE (approval_request_id)
);

CREATE INDEX admin_access_sessions_tenant_idx
    ON admin_access_sessions (tenant_id, expires_at);
CREATE INDEX admin_access_sessions_approval_idx
    ON admin_access_sessions (approval_request_id);
CREATE INDEX admin_access_sessions_expiry_idx
    ON admin_access_sessions (expires_at)
    WHERE revoked_at IS NULL AND expired_at IS NULL;

ALTER TABLE admin_access_sessions ENABLE ROW LEVEL SECURITY;
CREATE POLICY admin_access_sessions_isolation ON admin_access_sessions
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- DKIM keys
-- ================================================================

CREATE TABLE dkim_keys (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    domain_id             UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    selector              TEXT NOT NULL,
    public_key            TEXT NOT NULL,
    private_key_encrypted BYTEA NOT NULL,
    status                TEXT NOT NULL DEFAULT 'active'
                          CHECK (status IN ('active', 'deprecated', 'revoked')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at          TIMESTAMPTZ,
    expires_at            TIMESTAMPTZ,
    revoked_at            TIMESTAMPTZ,
    UNIQUE (domain_id, selector)
);

CREATE INDEX dkim_keys_domain_status_idx
    ON dkim_keys (domain_id, status);
CREATE INDEX dkim_keys_tenant_idx
    ON dkim_keys (tenant_id);

ALTER TABLE dkim_keys ENABLE ROW LEVEL SECURITY;
CREATE POLICY dkim_keys_isolation ON dkim_keys
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- WebAuthn + TOTP credentials
-- ================================================================

CREATE TABLE webauthn_credentials (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL,
    credential_id TEXT NOT NULL,
    public_key    TEXT NOT NULL,
    sign_count    BIGINT NOT NULL DEFAULT 0,
    name          TEXT NOT NULL DEFAULT 'Security key',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    UNIQUE (tenant_id, credential_id)
);

CREATE INDEX webauthn_credentials_user_idx
    ON webauthn_credentials (tenant_id, user_id);

ALTER TABLE webauthn_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY webauthn_credentials_isolation ON webauthn_credentials
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE totp_credentials (
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id              TEXT NOT NULL,
    encrypted_secret     BYTEA NOT NULL,
    recovery_codes_hash  TEXT NOT NULL DEFAULT '',
    enabled              BOOLEAN NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at         TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE INDEX totp_credentials_user_idx
    ON totp_credentials (tenant_id, user_id) WHERE enabled = TRUE;

ALTER TABLE totp_credentials ENABLE ROW LEVEL SECURITY;
CREATE POLICY totp_credentials_isolation ON totp_credentials
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Sieve rules
-- ================================================================

CREATE TABLE sieve_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id     TEXT,
    name        TEXT NOT NULL,
    script      TEXT NOT NULL,
    priority    INT  NOT NULL DEFAULT 100,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sieve_rules_tenant_priority_idx
    ON sieve_rules (tenant_id, priority, created_at);

CREATE TRIGGER sieve_rules_set_updated_at
    BEFORE UPDATE ON sieve_rules
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE sieve_rules ENABLE ROW LEVEL SECURITY;
CREATE POLICY sieve_rules_isolation ON sieve_rules
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- OAuth2 server (clients, codes, refresh, access tokens)
-- ----------------------------------------------------------------
-- KMail accepts two distinct token populations:
--
--   * OIDC user JWTs (the existing KChat-issued tokens). Verified
--     by `internal/middleware` against the JWKS at startup; this
--     is how the web/native clients authenticate human users.
--
--   * OAuth2 access tokens (this section). Issued to third-party
--     applications via the authorization code grant; bearer-only.
--     Verified by `internal/oauth.AuthMiddleware` against the
--     `oauth_access_tokens` table on every request. Tokens are
--     scoped (read:mail, write:mail, read:calendar, etc.) so a
--     third-party app cannot exceed the scopes the user granted
--     at authorization time.
--
-- The two populations carry independent tenant_id / user_id
-- contexts: a user JWT identifies the human; an OAuth2 token
-- identifies the *third-party app acting on behalf of* the human
-- that granted consent. The downstream API surface treats both
-- equivalently for RLS purposes (the same SetTenantGUC is
-- applied), but audit log entries distinguish the two so we can
-- attribute actions correctly.
--
-- Table ordering here resolves the
-- `oauth_access_tokens.refresh_token_id` → `oauth_refresh_tokens`
-- forward-reference inline. The original migration declared the
-- FK via a deferred ALTER TABLE because the two tables landed in
-- the same legacy migration; squashing lets us declare it inline.
-- ================================================================

-- oauth_clients: registered third-party applications. Each row
-- represents a single OAuth2 client (an application), not a user
-- session. Clients are per-tenant — a Zapier integration in
-- tenant A is a different row from the same integration in
-- tenant B, with its own client_secret hash.
--
-- redirect_uris stores the allow-list of callback URLs. The
-- /oauth/authorize handler rejects any redirect_uri that does
-- not exactly match an entry in this array (no prefix matching;
-- mismatched callback URLs are a known OAuth2 attack vector).
--
-- client_type captures whether the client is "confidential"
-- (a server-side app with a secret) or "public" (an SPA / mobile
-- app that cannot keep a secret). Public clients are required to
-- use PKCE; confidential clients may use PKCE or client_secret.
--
-- dispatch_quota_per_hour is a per-integration sliding-window
-- cap on outbound webhook deliveries; NULL means "use the global
-- default" (Service.DefaultClientDispatchPerHour). A tenant
-- administrator who needs to throttle a misbehaving integration
-- can set a low value here without changing the service-wide
-- ceiling.
CREATE TABLE oauth_clients (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id               TEXT NOT NULL UNIQUE,
    -- bcrypt hash of the plaintext client_secret. Only present
    -- for confidential clients; NULL for public clients.
    client_secret_hash      TEXT,
    client_type             TEXT NOT NULL CHECK (client_type IN ('confidential', 'public')),
    name                    TEXT NOT NULL,
    homepage_url            TEXT,
    logo_url                TEXT,
    redirect_uris           JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Allow-list of scopes this client can request. The actual
    -- scopes granted to a given access token are the intersection
    -- of this list, the scopes requested in the authorize URL,
    -- and the scopes the user approved on the consent screen.
    allowed_scopes          JSONB NOT NULL DEFAULT '[]'::jsonb,
    active                  BOOLEAN NOT NULL DEFAULT TRUE,
    dispatch_quota_per_hour INTEGER
                            CHECK (dispatch_quota_per_hour IS NULL OR dispatch_quota_per_hour > 0),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_oauth_clients_tenant ON oauth_clients(tenant_id);

ALTER TABLE oauth_clients ENABLE ROW LEVEL SECURITY;
CREATE POLICY rls_oauth_clients ON oauth_clients
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- oauth_authorization_codes: one-time-use codes issued by
-- /oauth/authorize and exchanged for tokens at /oauth/token.
--
-- code_challenge / code_challenge_method enforce PKCE per
-- RFC 7636. The token endpoint verifies code_verifier against
-- this challenge on exchange.
--
-- expires_at is bounded to 60 seconds per the OAuth2 spec
-- (RFC 6749 §4.1.2) — codes are short-lived because they're
-- bearer credentials that ride over the user's browser.
--
-- consumed_at flips to non-null on first /oauth/token exchange;
-- a second exchange with the same code MUST be rejected (and
-- per the spec, the originally-issued tokens revoked).
CREATE TABLE oauth_authorization_codes (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id              UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id                UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- SHA-256 hex hash of the plaintext code that was sent to the
    -- browser; the plaintext exists only in the redirect URL and
    -- in the third-party app's memory.
    code_hash              TEXT NOT NULL UNIQUE,
    redirect_uri           TEXT NOT NULL,
    granted_scopes         JSONB NOT NULL DEFAULT '[]'::jsonb,
    code_challenge         TEXT,
    code_challenge_method  TEXT CHECK (code_challenge_method IN ('plain', 'S256')),
    expires_at             TIMESTAMPTZ NOT NULL,
    consumed_at            TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_oauth_codes_expires ON oauth_authorization_codes(expires_at);

ALTER TABLE oauth_authorization_codes ENABLE ROW LEVEL SECURITY;
CREATE POLICY rls_oauth_codes ON oauth_authorization_codes
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- oauth_refresh_tokens: long-lived tokens used to mint new
-- access tokens without re-prompting the user. Subject to
-- refresh-token rotation: each /oauth/token call with a
-- refresh_token grant invalidates the old refresh token and
-- issues a new one, so a stolen refresh token is detectable
-- (the original owner's next use will fail, triggering full
-- revocation of the token family).
CREATE TABLE oauth_refresh_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id    UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The previous refresh-token row that this token was rotated
    -- from. NULL for the first refresh token in a chain (issued
    -- alongside the initial code exchange). On rotation, the
    -- previous row is marked revoked and the new row references
    -- it via this column. If a /oauth/token call presents a
    -- refresh_token that is already revoked AND has a successor,
    -- that's a replay attempt — revoke the entire successor
    -- chain.
    parent_id    UUID,
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_oauth_refresh_expires ON oauth_refresh_tokens(expires_at);
CREATE INDEX idx_oauth_refresh_parent  ON oauth_refresh_tokens(parent_id);

ALTER TABLE oauth_refresh_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY rls_oauth_refresh ON oauth_refresh_tokens
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- oauth_access_tokens: bearer tokens issued by /oauth/token.
-- Expires within an hour by default; clients refresh via the
-- refresh_token grant. The token plaintext is never stored —
-- we keep a SHA-256 hash so a database compromise does not
-- yield valid bearer tokens.
--
-- revoked_at supports the /oauth/revoke endpoint and the
-- automatic revocation triggered by code-reuse (RFC 6819).
--
-- refresh_token_id (FK to oauth_refresh_tokens) lets the
-- revocation cascade reach every access token a refresh token
-- ever minted. ON DELETE SET NULL is the correct semantic:
--   * CASCADE would mass-delete access tokens whenever a refresh
--     token row is hard-deleted (e.g. a DBA-run retention job).
--     Until expires_at fires, those access tokens are valid
--     bearer credentials — dropping the rows behind their back
--     would yield unexpected 401s without an audit trail.
--   * RESTRICT would block legitimate cleanup of expired
--     refresh-token rows whenever any old access token still
--     pointed at them, even though revoked_at had already done
--     the security work.
--   * SET NULL preserves the access-token row, breaks the
--     orphaned linkage, and makes the orphan visible.
CREATE TABLE oauth_access_tokens (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    client_id        UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash       TEXT NOT NULL UNIQUE,
    scopes           JSONB NOT NULL DEFAULT '[]'::jsonb,
    expires_at       TIMESTAMPTZ NOT NULL,
    revoked_at       TIMESTAMPTZ,
    refresh_token_id UUID REFERENCES oauth_refresh_tokens(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_oauth_tokens_expires           ON oauth_access_tokens(expires_at);
CREATE INDEX idx_oauth_tokens_user              ON oauth_access_tokens(user_id);
CREATE INDEX idx_oauth_tokens_refresh_token_id  ON oauth_access_tokens(refresh_token_id);

ALTER TABLE oauth_access_tokens ENABLE ROW LEVEL SECURITY;
CREATE POLICY rls_oauth_tokens ON oauth_access_tokens
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Tenant webhooks (Phase 5 + integrations OAuth2-owned variant)
-- ----------------------------------------------------------------
-- A webhook_endpoints row may be admin-owned (NULL oauth_client_id +
-- NULL user_id — legacy / operator-registered) or
-- integration-owned (both non-NULL). For integration-owned rows
-- the dispatcher filters event delivery by the intersection of
-- (a) the row's `events` JSONB allow-list and
-- (b) the consenting user's currently-granted scopes — computed
-- live from the union of non-revoked, non-expired
-- `oauth_access_tokens.scopes` for (tenant_id, client_id, user_id).
-- This closes the scope-leak gap where the static
-- `oauth_clients.allowed_scopes` (which scopes the client *may*
-- request) diverges from what the user *actually* granted at the
-- consent screen.
-- ================================================================

CREATE TABLE webhook_endpoints (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    events          JSONB NOT NULL DEFAULT '[]'::jsonb,
    secret_hash     TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    -- v1 (default): signs `t=<unix>,v1=<hex>` over `<unix>.<body>`.
    -- v2: adds a replay-protection nonce and emits per-delivery
    -- `X-KMail-Webhook-Nonce` and `X-KMail-Webhook-Timestamp`
    -- headers. Endpoints opt into v2 per-row.
    signing_version TEXT NOT NULL DEFAULT 'v1'
                    CHECK (signing_version IN ('v1', 'v2')),
    oauth_client_id UUID REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id         UUID REFERENCES users(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX webhook_endpoints_tenant_idx ON webhook_endpoints (tenant_id);
CREATE INDEX idx_webhook_endpoints_tenant_oauth_client
    ON webhook_endpoints (tenant_id, oauth_client_id)
    WHERE oauth_client_id IS NOT NULL;
CREATE INDEX idx_webhook_endpoints_tenant_client_user
    ON webhook_endpoints (tenant_id, oauth_client_id, user_id)
    WHERE oauth_client_id IS NOT NULL;

CREATE TRIGGER webhook_endpoints_set_updated_at
    BEFORE UPDATE ON webhook_endpoints
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
CREATE POLICY webhook_endpoints_isolation ON webhook_endpoints
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

CREATE TABLE webhook_deliveries (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    endpoint_id    UUID NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_type     TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts       INT NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    last_status    INT NOT NULL DEFAULT 0,
    next_retry_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at   TIMESTAMPTZ
);

CREATE INDEX webhook_deliveries_pending_idx
    ON webhook_deliveries (status, next_retry_at)
    WHERE status = 'pending';
CREATE INDEX webhook_deliveries_endpoint_idx
    ON webhook_deliveries (endpoint_id, created_at DESC);
CREATE INDEX webhook_deliveries_tenant_idx
    ON webhook_deliveries (tenant_id, created_at DESC);

ALTER TABLE webhook_deliveries ENABLE ROW LEVEL SECURITY;
CREATE POLICY webhook_deliveries_isolation ON webhook_deliveries
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Onboarding (manual checklist + auto-trigger log)
-- ================================================================

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

-- ================================================================
-- Global address list (CardDAV GAL cache)
-- ================================================================

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

-- ================================================================
-- Search cutover jobs (per-tenant, per-target-backend state machine)
-- ----------------------------------------------------------------
-- The composite PK on (tenant_id, target_backend) lets the same
-- tenant carry one completed and one pending row for different
-- targets simultaneously — required once more than one transition
-- (e.g. meilisearch → opensearch and shared_meilisearch →
-- shared_opensearch) is registered with the worker.
--
-- The auto-cutover worker runs with the control-plane GUC unset
-- (it iterates every tenant), so a permissive policy is required
-- here. RLS is still enabled so a stray tenant-scoped session
-- can't read another tenant's row.
-- ================================================================

CREATE TABLE search_cutover_jobs (
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    target_backend TEXT NOT NULL,
    cutover_state  TEXT NOT NULL DEFAULT 'pending'
                   CHECK (cutover_state IN ('pending', 'in_progress', 'completed', 'failed')),
    mailbox_size   BIGINT NOT NULL DEFAULT 0,
    threshold      BIGINT NOT NULL,
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    failure_count  INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, target_backend)
);

CREATE INDEX search_cutover_jobs_state_idx
    ON search_cutover_jobs (cutover_state, updated_at);

ALTER TABLE search_cutover_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY search_cutover_jobs_tenant_isolation ON search_cutover_jobs
    USING (
        current_setting('app.tenant_id', true) IS NULL
        OR current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    )
    WITH CHECK (
        current_setting('app.tenant_id', true) IS NULL
        OR current_setting('app.tenant_id', true) = ''
        OR tenant_id = current_setting('app.tenant_id', true)::uuid
    );

-- ================================================================
-- Alias → Stalwart sync queue
-- ----------------------------------------------------------------
-- The Tenant Service mirrors alias CRUD into Stalwart's principal
-- database (PATCH /api/principal/{name}). The BFF row is the
-- source of truth for the admin console; Stalwart sync is
-- best-effort. The service enqueues a row inside the same
-- transaction that writes / deletes the alias, then attempts
-- Stalwart sync inline. On inline success the row is marked
-- `synced`; on inline failure the row stays `pending` for the
-- AliasStalwartSyncWorker to retry with exponential backoff.
-- ================================================================

CREATE TABLE alias_stalwart_sync_queue (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    operation           TEXT NOT NULL
                        CHECK (operation IN ('add', 'remove')),
    stalwart_account_id TEXT NOT NULL,
    alias_email         TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'synced', 'failed')),
    attempts            INT NOT NULL DEFAULT 0,
    last_error          TEXT NOT NULL DEFAULT '',
    next_retry_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    synced_at           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX alias_stalwart_sync_queue_pending_idx
    ON alias_stalwart_sync_queue (next_retry_at)
    WHERE status = 'pending';
CREATE INDEX alias_stalwart_sync_queue_tenant_idx
    ON alias_stalwart_sync_queue (tenant_id, created_at DESC);

CREATE TRIGGER alias_stalwart_sync_queue_set_updated_at
    BEFORE UPDATE ON alias_stalwart_sync_queue
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE alias_stalwart_sync_queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY alias_stalwart_sync_queue_isolation
    ON alias_stalwart_sync_queue
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Scheduled Send queue
-- ================================================================

CREATE TABLE scheduled_sends (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    kchat_user_id       TEXT NOT NULL,
    stalwart_account_id TEXT NOT NULL,
    email_id            TEXT NOT NULL,        -- JMAP Email id (the draft)
    identity_id         TEXT NOT NULL,        -- JMAP Identity id
    submission          JSONB NOT NULL,       -- serialized EmailSubmission/set create args
    send_at             TIMESTAMPTZ NOT NULL, -- when the worker should dispatch
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'sent', 'cancelled', 'failed')),
    attempts            INT NOT NULL DEFAULT 0,
    last_error          TEXT NOT NULL DEFAULT '',
    next_retry_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX scheduled_sends_pending_idx
    ON scheduled_sends (send_at, next_retry_at)
    WHERE status = 'pending';
CREATE INDEX scheduled_sends_tenant_user_idx
    ON scheduled_sends (tenant_id, kchat_user_id, created_at DESC);

CREATE TRIGGER scheduled_sends_set_updated_at
    BEFORE UPDATE ON scheduled_sends
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE scheduled_sends ENABLE ROW LEVEL SECURITY;
CREATE POLICY scheduled_sends_tenant_isolation
    ON scheduled_sends
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Snoozed emails (Email Snooze worker)
-- ----------------------------------------------------------------
-- The active-row unique index scopes by (tenant, user, email_id)
-- not (tenant, email_id): KChat shared inboxes expose a single
-- backing Stalwart account to multiple users via MLS-group
-- decryption, so multiple users CAN see the same JMAP email_id
-- for the same underlying message. Each user must be able to
-- snooze their own copy independently.
-- ================================================================

CREATE TABLE snoozed_emails (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    kchat_user_id        TEXT NOT NULL,
    stalwart_account_id  TEXT NOT NULL,
    email_id             TEXT NOT NULL,            -- JMAP Email id
    original_mailbox_ids JSONB NOT NULL,           -- {"mb-inbox": true, ...}
    snoozed_mailbox_id   TEXT NOT NULL,
    snooze_until         TIMESTAMPTZ NOT NULL,
    mark_unread_on_wake  BOOLEAN NOT NULL DEFAULT TRUE,
    status               TEXT NOT NULL DEFAULT 'snoozed'
                         CHECK (status IN ('snoozed', 'unsnoozed', 'cancelled', 'failed')),
    attempts             INT NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    next_retry_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    woken_at             TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX snoozed_emails_pending_idx
    ON snoozed_emails (snooze_until, next_retry_at)
    WHERE status = 'snoozed';
CREATE INDEX snoozed_emails_tenant_user_idx
    ON snoozed_emails (tenant_id, kchat_user_id, created_at DESC);
CREATE UNIQUE INDEX snoozed_emails_active_unique
    ON snoozed_emails (tenant_id, kchat_user_id, email_id)
    WHERE status = 'snoozed';

CREATE TRIGGER snoozed_emails_set_updated_at
    BEFORE UPDATE ON snoozed_emails
    FOR EACH ROW EXECUTE FUNCTION kmail_set_updated_at();

ALTER TABLE snoozed_emails ENABLE ROW LEVEL SECURITY;
CREATE POLICY snoozed_emails_tenant_isolation
    ON snoozed_emails
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ================================================================
-- Seed data
-- ----------------------------------------------------------------
-- The five canonical IP pools and the local-dev default Stalwart
-- shard. Idempotent so re-applying on a partially-bootstrapped
-- database is safe.
-- ================================================================

INSERT INTO ip_pools (name, pool_type, description) VALUES
    ('system-transactional', 'system_transactional',
     'Platform notifications (password resets, DMARC reports).'),
    ('mature-trusted',       'mature_trusted',
     'Graduated tenants with clean reputation.'),
    ('new-warming',          'new_warming',
     'Default pool for new tenants during 30-day warmup ramp.'),
    ('restricted',           'restricted',
     'Reduced-volume pool for tenants under deliverability review.'),
    ('dedicated-enterprise', 'dedicated_enterprise',
     'Per-tenant dedicated IPs for enterprise add-on.')
ON CONFLICT (name) DO NOTHING;

INSERT INTO stalwart_shards (name, stalwart_url, postgres_dsn, max_mailboxes, status)
VALUES ('default', 'http://stalwart:8080', '', 5000, 'active')
ON CONFLICT (name) DO NOTHING;

COMMIT;
