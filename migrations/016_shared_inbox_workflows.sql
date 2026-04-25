-- KMail — Phase 4: Shared mailbox workflows.
--
-- Layered on top of the shared_inboxes table from
-- `migrations/001_initial_schema.sql`. Adds per-email assignment
-- + status columns and a parallel internal-notes timeline visible
-- only to shared-inbox members.

BEGIN;

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

COMMIT;
