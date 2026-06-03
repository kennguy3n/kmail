-- ================================================================
-- 003_export_artifacts.sql
-- ----------------------------------------------------------------
-- Phase: gap closure — eDiscovery export fan-out (Session 2).
--
-- Adds the artifact-metadata columns the export runner writes once
-- it has streamed an archive to zk-object-fabric, plus the
-- `export_job_messages` join table that records exactly which
-- JMAP message IDs were included in a given export (for audit /
-- legal-hold reproducibility).
--
-- `export_jobs` itself already ships in `001_baseline.sql`
-- (id, tenant_id, requester_id, format, scope, scope_ref, status,
-- download_url, error_message, created_at, started_at,
-- completed_at). This migration is additive only — no column is
-- dropped or retyped — so it satisfies the non-destructive
-- migration rule in docs/SCHEMA.md §7.
--
-- `download_url` (baseline) and `artifact_url` (here) coexist
-- deliberately: `download_url` historically held a short-lived
-- presigned GET; `artifact_url` holds the canonical, stable
-- object reference (s3://bucket/key) the runner persists so a
-- fresh presign can be minted on demand from the admin UI.
--
-- Idempotent: `ADD COLUMN IF NOT EXISTS` and `CREATE TABLE IF NOT
-- EXISTS` make a re-run a no-op on a database that already has the
-- baseline + this delta.
-- ================================================================

BEGIN;

ALTER TABLE export_jobs
    ADD COLUMN IF NOT EXISTS artifact_url        TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS artifact_size_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS artifact_checksum   TEXT   NOT NULL DEFAULT '';

-- export_job_messages: one row per message included in an export
-- archive. The lifecycle is owned entirely by the parent
-- export_jobs row (ON DELETE CASCADE), and every read/write is
-- reached through a tenant-scoped export_jobs lookup, so no
-- separate tenant_id / RLS policy is carried here — the rows are
-- unreachable except via a job the caller's tenant already owns.
CREATE TABLE IF NOT EXISTS export_job_messages (
    job_id      UUID NOT NULL REFERENCES export_jobs(id) ON DELETE CASCADE,
    message_id  TEXT NOT NULL,
    included_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, message_id)
);

CREATE INDEX IF NOT EXISTS export_job_messages_job_idx
    ON export_job_messages (job_id, included_at);

COMMIT;
