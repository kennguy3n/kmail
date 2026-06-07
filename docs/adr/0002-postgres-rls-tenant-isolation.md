# 0002 — PostgreSQL row-level security for tenant isolation

- **Status**: Accepted
- **Related**: [`../compliance/SECURITY_OVERVIEW.md`](../compliance/SECURITY_OVERVIEW.md) §2, [`../SCHEMA.md`](../SCHEMA.md), [`../../migrations/001_baseline.sql`](../../migrations/001_baseline.sql)

## Context

KMail is multi-tenant: one control-plane database holds rows for many
tenants. The single worst failure for a privacy product is a
cross-tenant data leak. Relying solely on every query carrying a
correct `WHERE tenant_id = …` is fragile — one missing predicate in one
handler is a breach.

## Decision

Enforce tenant isolation in the database with **PostgreSQL row-level
security (RLS)**. Tenant-scoped tables `ENABLE ROW LEVEL SECURITY` and
carry policies keyed on the current tenant (set per connection/session),
so the database itself refuses to return another tenant's rows even if
application code forgets a predicate. There is no cross-tenant query
path in the API.

## Consequences

- Isolation is defence-in-depth: a missing application-level filter
  cannot leak data because RLS still applies.
- Every connection must establish the tenant context before querying;
  pooling and the data-access layer are written around this invariant.
- New tenant-scoped tables MUST enable RLS and add a policy — this is a
  review checklist item, not optional. Tables added without it are a
  latent isolation gap.
- RLS adds a small per-query cost and constrains some query patterns,
  accepted as the price of hard isolation.

## Alternatives considered

- **Application-only filtering** — rejected: one bug = one breach.
- **Database-per-tenant** — rejected at this scale: operationally
  heavy (migrations, connection sprawl) for the tenant counts targeted;
  RLS gives strong isolation within a shared schema. Physical
  separation still exists at the mail-shard layer
  ([ADR 0006](./0006-stalwart-on-long-lived-hosts.md)).
