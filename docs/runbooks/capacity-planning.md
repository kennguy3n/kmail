# Runbook: Capacity planning

**Goal:** size shards, storage, and the control plane for the target of
**5,000 tenants across ~10 shards** (~500 mailboxes/shard), and decide
when to add capacity.

## 1. The shard model

A shard is one Stalwart StatefulSet pod + its PVC + a managed Postgres.
Tenants are routed to shards by the control plane; a shard stops
accepting new tenants at `max_mailboxes` (`stalwart_shards.max_mailboxes`,
set via Terraform `max_mailboxes` / `PUT /api/v1/admin/shards/{id}`).

```
shards_needed = ceil(total_mailboxes / mailboxes_per_shard)
```

At 5,000 tenants and the default planning figure of ~500 mailboxes per
shard, that's **10 shards**. Keep ~20% headroom: the auto-provisioner
(`KMAIL_SHARD_AUTOPROVISION_THRESHOLD`, default 0.8) stands up a new
shard once cluster utilisation crosses 80%.

## 2. Per-shard PVC sizing

The PVC holds Stalwart's **metadata store + FTS index + queue spool** —
NOT message bodies (those live in S3). Rule of thumb:

```
per_pod_GiB ≈ (mailboxes_per_shard * avg_msgs_per_mailbox * 2KiB) / 1Gi
              + queue_headroom + fts_rebuild_headroom
```

Worked example (defaults):

| Input | Value |
| --- | --- |
| mailboxes_per_shard | 500 |
| avg_msgs_per_mailbox | 20,000 |
| metadata/index per msg | ~2 KiB |
| raw index+meta | 500 × 20,000 × 2KiB ≈ **19 GiB** |
| + queue spool + FTS rebuild (≈1.5–2x) | headroom |
| **chart default `stalwart.storage.size`** | **50 GiB** |

`50Gi` leaves ~2x headroom for an online FTS rebuild. Use an SSD-backed,
topology-aware `storageClass` with `allowVolumeExpansion: true` so you
can grow without a migration.

## 3. Compute sizing (chart defaults)

| Workload | requests | limits | Scaling |
| --- | --- | --- | --- |
| kmail-api | 250m / 256Mi | 1000m / 1Gi | HPA 3→12 @ 70% CPU |
| kmail-worker | 100m / 128Mi | 500m / 512Mi | manual replicaCount |
| stalwart (per shard) | 500m / 1Gi | 2000m / 4Gi | manual replicaCount (NOT autoscaled) |

- **API**: ~300–400 req/s per replica before the p99 SLO degrades. Size
  HPA so steady-state keeps the average replica under ~60% CPU, leaving
  burst room to the 70% target.
- **Stalwart**: memory-bound (FTS + page cache); budget ~1Gi per ~150
  active mailboxes before raising the limit. **Add shards**, don't grow a
  single shard unbounded.

## 4. Managed dependency sizing

- **Postgres** (`modules/postgres`): start `db-4vcpu-16gb`, `200Gi`,
  `ha_enabled=true`. Watch connections + WAL; scale class before you hit
  CPU/IO saturation.
- **Valkey** (`modules/valkey`): `cache-2vcpu-4gb` with ≥1 replica
  covers rate-limit + cache for the fleet. It fails CLOSED in prod, so
  treat it as tier-0 — never run it single-node in production.
- **Object store** (`modules/object-store`): Wasabi scales transparently;
  watch egress cost via the cost-per-tenant dashboard.

## 5. When to add a shard

Add a shard when ANY of:
- Cluster mailbox utilisation > 80% (the auto-provisioner does this
  automatically if `KMAIL_SHARD_AUTOPROVISION_*` is configured).
- A shard's p99 JMAP latency trends up under steady mailbox count
  (vertical limit reached).
- A shard PVC is past ~70% used and growth is steady.

```bash
# Current utilisation across shards (list returns {"shards":[...]}):
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" "$API/api/v1/admin/shards" \
  | jq '.shards[] | {id, current_mailboxes, max_mailboxes,
                     util: (.current_mailboxes / .max_mailboxes)}'

# Or the ready-made cluster report (CapacityReport):
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards/health" | jq '{cluster_utilised, needs_provisioning}'
```

Manual provision (the auto-provisioner calls the same path):

```bash
./scripts/provision-shard.sh shard-$(date +%s)
# then register it (POST /api/v1/admin/shards) if not auto-registered.
```

## 6. Growth checkpoints

| Tenants | Shards | Notes |
| --- | --- | --- |
| 500 | 1–2 | Single-region is fine. |
| 2,500 | ~5 | Confirm cross-AZ spread is real (`kubectl get pods -o wide`). |
| 5,000 | ~10 | Target. Revisit Postgres class + Valkey replicas. |
| 10,000+ | 20+ | Consider multi-region (values-multiregion.yaml) + per-region control plane. |
