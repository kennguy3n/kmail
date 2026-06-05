-- ================================================================
-- 006_feature_flags.sql
-- ----------------------------------------------------------------
-- Phase: WS4 platform reliability — PostgreSQL-backed feature flag
-- store (Task 1). This is the highest-priority WS4 deliverable
-- because the other workstreams gate their rollouts on it via
-- `featureflags.IsEnabled(ctx, "...")`.
--
-- Two tables:
--
--   * `feature_flags` is the flag *registry*: one row per known
--     flag with its human description and the fallback value used
--     when no scoped override matches (`default_enabled`).
--
--   * `feature_flag_overrides` holds scoped rollout rules. Each row
--     forces a flag on/off for one scope:
--       - global  (scope_id = '')      — overrides the registry default
--       - plan    (scope_id = plan)    — 'core' | 'pro' | 'privacy'
--       - tenant  (scope_id = tenant UUID text)
--       - user    (scope_id = kchat_user_id text)
--     Evaluation precedence (most specific wins):
--       user > tenant > plan > global > flag default.
--
-- These are CONTROL-PLANE tables, mirroring `stalwart_shards`:
-- they are read across every tenant by the resolver (and by the
-- worker process, which runs with the `app.tenant_id` GUC unset),
-- and they are written only through the admin API. They therefore
-- intentionally carry NO row-level-security policy — a per-tenant
-- RLS predicate would hide the global / plan rows and break
-- cross-tenant resolution. Tenant isolation for *reads* happens in
-- the application layer: `IsEnabled` only ever consults overrides
-- whose scope_id matches the calling tenant/user.
--
-- Idempotent: tables/indexes guarded with IF NOT EXISTS. Additive
-- only — no existing object is altered or dropped.
-- ================================================================

BEGIN;

CREATE TABLE IF NOT EXISTS feature_flags (
    key             TEXT PRIMARY KEY
                    CHECK (key <> '' AND length(key) <= 128),
    description     TEXT NOT NULL DEFAULT '',
    default_enabled BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS feature_flag_overrides (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flag_key    TEXT NOT NULL REFERENCES feature_flags(key) ON DELETE CASCADE,
    scope       TEXT NOT NULL
                CHECK (scope IN ('global', 'plan', 'tenant', 'user')),
    -- scope_id is the empty string for the singleton global scope and
    -- the plan name / tenant UUID / user id otherwise. A CHECK keeps
    -- the global scope a true singleton (empty id) and the others
    -- non-empty so a malformed override can never silently shadow the
    -- global rule.
    scope_id    TEXT NOT NULL DEFAULT ''
                CHECK (
                    (scope = 'global' AND scope_id = '')
                    OR (scope <> 'global' AND scope_id <> '')
                ),
    enabled     BOOLEAN NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (flag_key, scope, scope_id)
);

CREATE INDEX IF NOT EXISTS feature_flag_overrides_flag_idx
    ON feature_flag_overrides (flag_key);

COMMIT;
