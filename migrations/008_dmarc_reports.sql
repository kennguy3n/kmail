-- KMail — Phase 3: DMARC aggregate report ingestion.
--
-- Stores RFC 7489 aggregate XML reports uploaded or parsed out of
-- the DMARC reporting mailbox so tenant admins can see which
-- sources are passing / failing DKIM + SPF alignment.

BEGIN;

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

COMMIT;
