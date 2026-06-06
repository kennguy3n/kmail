# KMail operational runbooks

Step-by-step procedures for operating KMail in production (5,000
tenants across ~10 shards). Each runbook is self-contained and assumes
you have:

- `kubectl` access to the production cluster (namespace `kmail`).
- An admin OIDC token for the BFF admin API
  (`/api/v1/admin/*`). Export it as `$KMAIL_ADMIN_TOKEN`; every `curl`
  below sends `Authorization: Bearer $KMAIL_ADMIN_TOKEN`.
- `$API` set to the API base URL (e.g. `https://api.example.com`).

| Runbook | When to use |
| --- | --- |
| [shard-failover.md](shard-failover.md) | A Stalwart shard is down / degraded and tenants on it can't reach mail. |
| [tenant-migration.md](tenant-migration.md) | Move one or more tenants from one shard to another (rebalancing / evacuation). |
| [database-backup-restore.md](database-backup-restore.md) | Take/verify backups and restore Postgres (control-plane or per-shard). |
| [stalwart-upgrade.md](stalwart-upgrade.md) | Upgrade the Stalwart mail core across the fleet. |
| [incident-response.md](incident-response.md) | An alert fired / users report an outage; triage and mitigate. |
| [capacity-planning.md](capacity-planning.md) | Size shards/storage and decide when to add a shard. |

## Shard admin API quick reference

The control plane exposes shard operations under `/api/v1/admin/shards`
(see `internal/tenant/shard_handlers.go`):

| Method & path | Action |
| --- | --- |
| `GET /api/v1/admin/shards` | List all shards. Returns `{"shards":[...]}` (each: id, name, stalwart_url, current_mailboxes, max_mailboxes, status). |
| `GET /api/v1/admin/shards/health` | Cluster `CapacityReport`: `{"shards":[{id,name,status,mailboxes,max_mailboxes,utilised}], cluster_utilised, needs_provisioning, ...}`. |
| `POST /api/v1/admin/shards` | Register a new shard. |
| `GET /api/v1/admin/shards/{id}` | Get one shard + its tenants: `{"shard":{...},"tenants":[...]}`. |
| `PUT /api/v1/admin/shards/{id}` | Update a shard: set `status` (`active`/`draining`/`offline`), change `max_mailboxes`. |
| `POST /api/v1/admin/shards/{id}/rebalance` | Move a tenant TO this shard (`{id}` is the destination; body `{"from_shard_id":"...","tenant_id":"..."}`). |

> Setting a shard to `draining` (or `offline`) via `PUT` stops the
> signup router / auto-provisioner from assigning **new** tenants to it
> (`AssignTenantToShard` only picks `active` shards); it does not move
> existing tenants (use `tenant-migration.md` for that).
