# KMail — PostgreSQL Schema Design

**License**: Proprietary — All Rights Reserved. See [LICENSE](../LICENSE).

> Status: Phase 1 — Foundation. This document describes the initial
> PostgreSQL schema owned by the Go control plane. The canonical
> DDL lives in
> [migrations/001_initial_schema.sql](../migrations/001_initial_schema.sql).
> See [ARCHITECTURE.md §4](ARCHITECTURE.md) for the storage topology
> and [ARCHITECTURE.md §6](ARCHITECTURE.md) for the multi-tenancy
> model.

---

## 1. Scope

The schema in this document covers **control-plane metadata** owned
by the Go services:

- Tenants, domains, users, aliases, shared inboxes.
- Quotas, plans, seat counts.
- Tenant audit log.
- Calendar metadata (not the events themselves; those live in
  Stalwart's CalDAV store).

It does **not** cover Stalwart's own mailbox / message / folder
state. Stalwart owns its own Postgres schema (prefixed `sw_`) in
the same database, and those tables are managed by Stalwart's
migrations, not ours.

---

## 2. Design Principles

1. **Row-level tenant isolation.** Every tenant-scoped table has a
   `tenant_id UUID NOT NULL` column with a `NOT NULL` FK to
   `tenants(id)` and an index on `tenant_id`. Go services set
   `app.tenant_id` as a session GUC on every connection and
   Postgres row-level-security (RLS) policies enforce the scope.
2. **UUID primary keys.** Go services generate UUIDv7
   (monotonic, sortable, safe for external exposure) client-side
   for all inserts they perform — that is the preferred path. The
   DDL also declares `DEFAULT gen_random_uuid()` on `id` columns
   as a fallback for ad-hoc SQL inserts (seed data, manual
   operator actions) so the schema is usable without the
   application layer. `gen_random_uuid()` returns UUIDv4, which
   loses the monotonic-ordering property but preserves uniqueness
   and foreign-key integrity. No server-generated sequences are
   used for primary keys.
3. **No hard deletes for tenant-affecting records.** Rows carry a
   `status` column (`active`, `suspended`, `deleted`) rather than
   being removed, so audit trails remain consistent. Housekeeping
   jobs vacuum `deleted` rows on a retention schedule.
4. **Timestamps always `timestamptz`.** Stored in UTC. `created_at`
   and `updated_at` on every mutable row. `updated_at` is
   maintained by a trigger.
5. **JSONB for extensible metadata** (audit log, calendar ACLs).
   Core queryable fields stay as columns; only open-ended payloads
   go into JSONB.
6. **Tenant-scoped uniqueness.** Emails, aliases, and addresses
   are unique across the system (mail must resolve globally), but
   domain verification flags and shared inbox addresses live inside
   a tenant namespace.
7. **FK cascade policy.** `ON DELETE RESTRICT` by default.
   Tenant-level cleanup is done via the Tenant Service, not via
   cascading deletes, so we never silently drop mail state when an
   admin removes a domain.

---

## 3. Table Overview

| Table                  | Purpose                                                              | Owner service          |
| ---------------------- | -------------------------------------------------------------------- | ---------------------- |
| `tenants`              | Organization (tenant) records.                                       | Tenant Service         |
| `users`                | KMail user accounts mapped to KChat identities.                      | Tenant Service         |
| `domains`              | Tenant-owned sending domains with DNS verification flags.            | DNS Onboarding         |
| `aliases`              | Email aliases routed to a user.                                      | Tenant Service         |
| `shared_inboxes`       | Shared inbox addresses mapped to MLS groups.                         | Tenant Service         |
| `shared_inbox_members` | Membership join table between users and shared inboxes.              | Tenant Service         |
| `quotas`               | Tenant-level storage and seat counters.                              | Billing Service        |
| `audit_log`            | Tamper-evident audit log of tenant-affecting actions.                | Audit / Compliance API |
| `calendar_metadata`    | Metadata for personal / team / resource calendars.                   | Calendar Bridge        |

---

## 4. Row-Level Security

Every tenant-scoped table carries an RLS policy:

```sql
ALTER TABLE <table> ENABLE ROW LEVEL SECURITY;

CREATE POLICY <table>_tenant_isolation ON <table>
    USING (tenant_id = current_setting('app.tenant_id')::uuid)
    WITH CHECK (tenant_id = current_setting('app.tenant_id')::uuid);
```

The `tenants` table itself is not tenant-scoped — callers with the
operator role bypass RLS explicitly via `BYPASSRLS` for tenant
provisioning. The Go services use separate Postgres roles for
tenant-scoped reads and operator-level writes so misconfiguration
cannot accidentally leak cross-tenant data.

---

## 5. Table-by-Table Reference

### 5.1 `tenants`

- `id` UUID primary key.
- `name` TEXT — human-readable display name.
- `slug` TEXT UNIQUE — URL-safe tenant identifier.
- `plan` TEXT — one of `core`, `pro`, `privacy`.
- `status` TEXT — `active`, `suspended`, `deleted`.
- `created_at`, `updated_at` TIMESTAMPTZ.

Authoritative source for all tenant metadata. Referenced by every
other tenant-scoped table.

### 5.2 `users`

- `id` UUID primary key.
- `tenant_id` UUID FK → `tenants(id)`.
- `email` TEXT UNIQUE — global primary address.
- `display_name` TEXT.
- `role` TEXT — `owner`, `admin`, `member`, `billing`,
  `deliverability`.
- `status` TEXT — `active`, `suspended`, `deleted`.
- `quota_bytes` BIGINT — per-user mailbox quota.
- `created_at` TIMESTAMPTZ.

KMail user records map 1:1 to KChat user identities. The BFF
resolves `(tenant_id, kchat_user_id) → stalwart_account_id` via
this table (see [JMAP-CONTRACT.md §3.3](JMAP-CONTRACT.md)).

### 5.3 `domains`

- `id` UUID primary key.
- `tenant_id` UUID FK.
- `domain` TEXT UNIQUE — fully qualified domain name.
- `verified` BOOLEAN — overall verification state.
- `mx_verified`, `spf_verified`, `dkim_verified`, `dmarc_verified`
  BOOLEAN — per-record verification flags.
- `created_at` TIMESTAMPTZ.

The DNS Onboarding service writes verification flags as it walks
the DNS wizard (see [PROPOSAL.md §9.3](PROPOSAL.md)). The `domain`
column is globally unique because two tenants cannot claim the same
domain simultaneously.

### 5.4 `aliases`

- `id` UUID primary key.
- `tenant_id` UUID FK.
- `user_id` UUID FK → `users(id)`.
- `alias_email` TEXT UNIQUE.
- `created_at` TIMESTAMPTZ.

Aliases route to a user's primary address. Deleting a user cascades
to deleting aliases via the Tenant Service's soft-delete flow (not
via DB cascade).

### 5.5 `shared_inboxes`

- `id` UUID primary key.
- `tenant_id` UUID FK.
- `address` TEXT — shared inbox address (e.g., `sales@acme.com`).
- `display_name` TEXT.
- `mls_group_id` TEXT — identifier of the KChat MLS group that
  gates access.
- `created_at` TIMESTAMPTZ.

The `(tenant_id, address)` pair is unique within a tenant. The
global uniqueness of `address` is enforced by Stalwart's SMTP
routing rather than by this table (because the same address cannot
route to two mailboxes).

### 5.6 `shared_inbox_members`

- `tenant_id` UUID FK → `tenants(id)`. Denormalized from
  `shared_inboxes.tenant_id` so RLS can be enforced directly on
  this join table; PostgreSQL RLS on the referenced
  `shared_inboxes` row does not propagate to rows in the
  referencing table.
- `shared_inbox_id` UUID FK → `shared_inboxes(id)`.
- `user_id` UUID FK → `users(id)`.
- `role` TEXT — `owner`, `member`, `viewer`.
- `added_at` TIMESTAMPTZ.
- Composite primary key `(shared_inbox_id, user_id)`.

The Tenant Service must ensure `tenant_id` matches
`shared_inboxes.tenant_id` and `users.tenant_id` on insert; a
future migration may add a trigger or composite FK to enforce
this at the database level.

A change to this table triggers an MLS epoch change on the shared
inbox group (out-of-band, by the Tenant Service).

### 5.7 `quotas`

- `tenant_id` UUID primary key + FK.
- `storage_used_bytes`, `storage_limit_bytes` BIGINT.
- `seat_count`, `seat_limit` INT.

The Billing Service is authoritative. The BFF's quota view
(`urn:ietf:params:jmap:quota`) is a read-through of this table.

### 5.8 `audit_log`

- `id` UUID primary key.
- `tenant_id` UUID FK.
- `actor_id` UUID — user or system principal that performed the
  action. Nullable for system-generated events.
- `action` TEXT — canonical action name (`tenant.create`,
  `domain.verify`, `user.suspend`, `mailbox.acl.update`, …).
- `resource_type` TEXT — `tenant`, `user`, `domain`, `alias`,
  `shared_inbox`, `quota`, `calendar`, `mailbox`.
- `resource_id` UUID — target record.
- `metadata` JSONB — event-specific payload (before/after diff,
  request correlation ID, IP address, user agent).
- `created_at` TIMESTAMPTZ.

The audit log is append-only. A nightly job seals the previous
day's rows with a hash chain so tampering is detectable.

### 5.9 `calendar_metadata`

- `id` UUID primary key.
- `tenant_id` UUID FK.
- `owner_id` UUID FK → `users(id)`.
- `calendar_type` TEXT — `personal`, `team`, `resource`.
- `name` TEXT.
- `acl` JSONB — `{ "readers": [user_id], "writers": [user_id],
  "admins": [user_id] }`.
- `created_at` TIMESTAMPTZ.

Mirrors the subset of CalDAV calendar state needed for RBAC
decisions in the BFF and Admin API. Events themselves remain in
Stalwart's CalDAV store.

---

## 6. Indexing Strategy

- `tenant_id` B-tree on every tenant-scoped table.
- Unique indexes as described above (`users.email`,
  `domains.domain`, `aliases.alias_email`, `tenants.slug`).
- `audit_log(tenant_id, created_at DESC)` for dashboard queries.
- `audit_log(tenant_id, resource_type, resource_id)` for
  per-resource history lookups.
- `shared_inbox_members(tenant_id)` for RLS-scoped scans.
- `shared_inbox_members(user_id)` for "what shared inboxes does
   this user belong to" reverse lookups.
- Partial index on `domains(tenant_id) WHERE verified = true` for
  the common "list active domains" query.

---

## 7. Migrations

- Migration files live in `migrations/NNN_<slug>.sql` using a
  simple numeric prefix. The `kmail-migration` CLI applies them
  idempotently against a `schema_migrations` tracking table.
- No destructive migrations without a two-step deploy (first
  stop writing, then drop). This is enforced by review — the
  migration tool does not distinguish, but destructive DDL must
  have an accompanying rollout note in the commit.
- Stalwart's migrations run separately; we never co-mingle schema
  ownership.

---

## 8. Backup and Retention

- Postgres backups are handled at the infrastructure layer (PITR
  on the managed Postgres service). The schema does not encode
  retention; it is a platform concern.
- The audit log's append-only structure is retained for the
  tenant's plan retention period (default 2 years, extended on
  Privacy / Enterprise plans) and archived to zk-object-fabric
  cold storage thereafter.
