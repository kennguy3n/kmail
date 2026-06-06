# Capacity planning guide

Sizing guidance for a KMail deployment. The defaults in
[`values.yaml`](../../deploy/helm/kmail/values.yaml) are a sensible
starting point; tune from the metrics in the
[monitoring guide](./monitoring.md).

## Control plane (`kmail-api`)

The BFF is stateless, so scale it horizontally. Chart defaults:

| Knob | Default | Notes |
| ---- | ------- | ----- |
| `kmailApi.replicaCount` | `3` | Floor for HA across zones. |
| `kmailApi.hpa.minReplicas` / `maxReplicas` | `3` / `12` | Autoscale on CPU. |
| `kmailApi.hpa.targetCPUUtilizationPercentage` | `70` | |
| `kmailApi.resources.requests` | `250m` CPU / `256Mi` | |
| `kmailApi.resources.limits` | `1000m` CPU / `1Gi` | |
| `kmailApi.pdb.minAvailable` | `1` | Keeps capacity during rollouts. |

Scale guidance:

- Watch `kmail_http_request_duration_seconds` p99 and CPU. If the HPA
  is pinned at `maxReplicas` with healthy latency, raise `maxReplicas`;
  if latency degrades before CPU saturates, raise per-pod CPU limits or
  investigate a downstream (JMAP proxy / Postgres) bottleneck.
- For zone/region resilience set `topologySpreadConstraints` so a
  single AZ outage can't take down every replica.

## Worker (`kmail-worker`)

The worker defaults to **1 replica** with `100m`/`128Mi` requests
(`500m`/`512Mi` limits). Every worker claims its unit of work via a
Postgres advisory/row lock (`FOR UPDATE SKIP LOCKED`) or a Valkey
lease, so running more than one replica is safe but usually unnecessary.
Scale up only when a specific worker's queue backs up. Watch
`kmail_worker_restarts_total` and `kmail_worker_panics_total` — the
supervisor restarts crashed workers forever with capped backoff, so a
permanently broken worker shows up as a restart/panic rate, not a pod
crash.

## Stalwart mail shards

Stalwart is **not** autoscaled — mail nodes are long-lived for IP
reputation. Capacity is added by sizing nodes and adding shards.

- **Per node** (chart `stalwart.resources`, applicable to VM sizing
  too): start at `500m`/`1Gi` requested, `2000m`/`4Gi` limit, with
  `50Gi` of fast storage (`stalwart.storage.size`).
- **Shard count**: each shard is a Postgres logical group of tenants
  with 2+ nodes (1 primary + warm secondaries). Add a shard when a
  shard's node CPU/IO or mailbox storage approaches its ceiling, or to
  isolate a noisy/large tenant. Tenants are assigned capacity-aware via
  `ShardService.AssignTenantToShard`.
- **Failover**: the BFF circuit-breaks a host after
  `KMAIL_PROXY_CIRCUIT_THRESHOLD` consecutive failures (default `3`)
  and routes to the next backup by `shard_failover_config.priority`
  until the shard-health worker (60s probe) marks it healthy again.

## Outbound IP pools

Deliverability depends on IP reputation, which constrains how fast you
can grow outbound volume:

- Allocate **one stable outbound IP per shard per outbound pool**. Do
  not share an IP across unrelated pools (e.g. the privacy-tier pool
  must not carry core-tier traffic — see `docs/POLICY.md` §3).
- Set the PTR to `mta-{shard}-{node}.{cluster}.example.com` and ensure
  forward-confirmed reverse DNS, or receivers will downrank mail.
- New IPs warm up over `KMAIL_DELIVERABILITY_WARMUP_DAYS` (default
  `14`). Plan added send capacity around the warmup ramp, not instantly.

## Shared backing services

- **PostgreSQL 16** — the control-plane metadata store and Stalwart's
  data store. Size for connection count (BFF replicas × pool size +
  worker + Stalwart nodes) and IOPS; this is the component most
  sensitive to under-provisioning. See [backup & restore](./backup-restore.md).
- **Valkey 8** — short-TTL state (rate limits, undo-send buffer,
  circuit-breaker, sessions, push queue). Memory-bound; size for peak
  concurrent state, not long-term storage.
- **zk-object-fabric (S3)** — blob storage; grows with mailbox
  attachment volume.
- **Meilisearch / OpenSearch** — the search tier. Rebuildable from
  source data, but size for index footprint and query latency.
