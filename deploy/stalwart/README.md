# KMail — Stalwart Multi-Node HA Operator Guide

This directory holds the production deployment templates for the
multi-node Stalwart cluster KMail rides on. The dev `docker-compose.yml`
runs a single Stalwart container; production splits Stalwart into
shards (one shard = one Postgres logical group of tenants), each shard
backed by N stateful nodes behind a load balancer.

> **Constraint reminder.** Stalwart MUST run on long-lived hosts (VMs
> or bare metal). Do not deploy Stalwart on Kubernetes pods —
> long-lived SMTP IPs require IP reputation that survives node
> rescheduling. See `docs/ARCHITECTURE.md` §11.

## Shard topology

```
                ┌────── kmail-bff (Go, JMAP proxy) ──────┐
                │                                        │
   client ──►   │  /jmap/*  →  shard A primary  ─►  Stalwart-A1
                │           ↘  shard A backup   ─►  Stalwart-A2 (warm)
                │  resolved per tenant via tenant_shard_assignments
                │  + shard_failover_config
                │                                        │
                │  /jmap/*  →  shard B primary  ─►  Stalwart-B1
                └────────────────────────────────────────┘
                            │
                            ▼
                shared Postgres / shared zk-object-fabric /
                shared Meilisearch / shared Valkey
```

* Each shard has 2+ nodes for HA. One node is the primary; the rest
  are warm secondaries backed by the same shared stores.
* Tenants pin to a shard via `tenant_shard_assignments`. The JMAP
  proxy (`internal/jmap/proxy.go`) resolves the primary URL per
  request and consults `shard_failover_config` for backups when the
  primary returns 5xx.
* Backup ordering is set by `shard_failover_config.priority` (lower
  wins). The proxy circuit-breaks a host after
  `KMAIL_PROXY_CIRCUIT_THRESHOLD` consecutive failures (default 3)
  and routes to the next backup until the shard health worker probes
  the host healthy again.

## Per-shard config

Use `ha-config.json` as the per-node template. Replace the
`REPLACE_*` tokens with shard-specific values (node ID, PTR record,
outbound stable IP, shard name). Common values across the whole
cluster (Postgres URL, Meilisearch, Valkey) come from environment
variables to make secret rotation a single Ansible / Terraform
update rather than a JSON edit.

### Required env per node

| Variable | Purpose |
|----------|---------|
| `STALWART_S3_ACCESS_KEY` | Per-tenant bucket credentials provisioned by `internal/tenant/zkfabric.go`. The deployment automation reads `tenant_storage_credentials` from Postgres and rewrites the per-shard blob-store record before the shard accepts traffic. |
| `STALWART_S3_SECRET_KEY` | (paired with the access key) |
| `STALWART_MEILISEARCH_KEY` | Shared Meilisearch master key (read-write). |
| `STALWART_VALKEY_URL` | Shared Valkey URL for rate-limit / push queue. |

### IP reputation

* Allocate one stable outbound IP per shard per outbound pool.
* Set the PTR record to `mta-{shard}-{node}.{cluster}.example.com`.
  This MUST resolve forward to the IP (forward-confirmed reverse
  DNS) or major receivers will downrank the mail.
* Do NOT share an outbound IP across unrelated pools (e.g. dedicated
  privacy-tier pool MUST NOT serve core-tier traffic — see
  `docs/POLICY.md` §3).
* New IPs warm up over `KMAIL_DELIVERABILITY_WARMUP_DAYS` (default
  14) per the deliverability service.

### Health checks

Each node exposes `/healthz` (`200 OK` when ready). The KMail
shard health worker (`tenant.HealthWorker`) probes every shard
every 60s and writes the result to
`stalwart_shards.healthy`. The JMAP proxy uses that flag plus its
in-process circuit-breaker to skip degraded hosts.

### Load balancer

Front the JMAP listener with a TCP/HTTP load balancer (HAProxy,
NGINX, AWS ALB, ...). Disable session affinity on the LB — the
KMail BFF already does shard-level pinning. The LB must:

1. Terminate TLS or proxy ACME challenges through to Stalwart.
2. Health-check `/healthz`.
3. Accept the JMAP `Connection: upgrade` for EventSource pushes.

### Provisioning a new tenant on a shard

1. KMail BFF runs `tenant.NewService.CreateTenant`. The lifecycle
   hooks call `ZKFabricProvisioner.Provision` which mints the
   per-tenant bucket + S3 keys + placement policy.
2. The shard service assigns the tenant to a shard via
   `ShardService.AssignTenantToShard` (capacity-aware).
3. The deployment automation reads `tenant_storage_credentials` and
   POSTs a per-tenant `BlobStore` record onto every node in the
   assigned shard via Stalwart's JMAP admin surface (mirrors
   `scripts/stalwart-init.sh`).
4. Shard health worker confirms the new tenant routes correctly,
   then the BFF starts accepting traffic.

### Disaster failover

The BFF JMAP proxy retries against the configured backup
shard URLs in priority order without operator intervention. To
manually drain a shard for maintenance:

```sh
# Mark the primary unhealthy. The shard health worker will keep
# probing and reset the flag once the host comes back.
PUT /api/v1/admin/shards/{shard_id}/health -d '{"healthy":false}'
```

After the primary is drained, traffic flows to the next-priority
backup until the maintenance window ends.

### Capacity planning

* Each Stalwart shard handles up to `KMAIL_SHARD_TENANT_CAP` tenants
  (default 1000). The shard service enforces the cap during
  `AssignTenantToShard` and surfaces `ErrNoCapacity` when full.
* Shard nodes scale vertically; horizontal scale-out happens by
  adding new shards and migrating tenants via
  `ShardService.RebalanceShard`.

### Pinned versions

* Stalwart: **v0.16.0** (do NOT auto-upgrade — see
  `docs/PROGRESS.md`). v1.0.0 is expected H1 2026 and will be
  introduced through a staging plan, not a rolling deploy.
* PostgreSQL: 15+
* Meilisearch: 1.7+
* Valkey: 7.x
