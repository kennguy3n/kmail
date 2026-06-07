# Runbook: Shard failover

**Goal:** restore mail service for tenants on a Stalwart shard that is
down or degraded, with minimal data loss.

**Symptoms / triggers:**
- Alert `KmailTargetDown` (a stalwart target), or `KmailErrorBudgetBurnFast`.
- `GET /api/v1/admin/shards/health` reports a shard with `status` not
  `active` (e.g. `offline`) or a high `utilised`.
- Tenants on one shard see JMAP errors; the per-shard health dashboard
  panel is red.

## 0. Preconditions / context

Each shard is one Stalwart **StatefulSet** pod (`kmail-stalwart-<n>`)
with its own PVC (the metadata/index/spool volume) and its own managed
Postgres. Message bodies live in the shared S3 blob store, **not** on
the pod. A shard has a primary plus optional secondaries
(`GetSecondaryShards`). Recovery strategy depends on whether the PVC and
Postgres survived.

## 1. Confirm scope (2 min)

```bash
# Which shard, and is it really down?  The health endpoint returns a
# CapacityReport: {"shards":[{id,name,status,mailboxes,max_mailboxes,
# utilised}], cluster_utilised, needs_provisioning, ...}.
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards/health" | jq '.shards[] | {id, status, utilised}'

kubectl -n kmail get pods -l app.kubernetes.io/component=stalwart -o wide
kubectl -n kmail describe pod kmail-stalwart-<n> | tail -40
```

Decide the failure class:
- **A. Pod down, PVC + Postgres intact** (node drain, OOM, crashloop) → §2.
- **B. PVC lost, Postgres intact** (volume/AZ failure) → §3.
- **C. Postgres down** → run `database-backup-restore.md` first, then §2.

## 2. Class A — reschedule the pod

```bash
# Stop new tenants landing on the shard while you work. Setting status
# to `draining` keeps existing tenants served but excludes the shard
# from new-tenant assignment (AssignTenantToShard only picks `active`).
curl -s -X PUT -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  "$API/api/v1/admin/shards/<shard-id>" -d '{"status": "draining"}'

# Inspect, then let the StatefulSet reschedule.
kubectl -n kmail logs kmail-stalwart-<n> --previous | tail -100
kubectl -n kmail delete pod kmail-stalwart-<n>   # StatefulSet recreates it
kubectl -n kmail rollout status statefulset/kmail-stalwart --timeout=300s
```

If the pod is stuck `Pending` on volume affinity, the PVC is bound to an
AZ whose nodes are gone → treat as Class B.

Verify and re-enable:

```bash
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards/health" | jq '.shards[] | select(.id=="<shard-id>")'
curl -s -X PUT -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  "$API/api/v1/admin/shards/<shard-id>" -d '{"status": "active"}'
```

## 3. Class B — PVC lost, rebuild from blob store + Postgres

The durable truth is Postgres (mailbox/UID metadata) + S3 (bodies). The
PVC is a rebuildable cache/index.

1. Keep the shard `draining` (step in §2).
2. Provision a replacement volume in a healthy AZ. If the PVC is
   unrecoverable, delete the PVC + pod so the `volumeClaimTemplate`
   recreates an empty volume in a schedulable zone:
   ```bash
   kubectl -n kmail delete pvc data-kmail-stalwart-<n>
   kubectl -n kmail delete pod kmail-stalwart-<n>
   kubectl -n kmail rollout status statefulset/kmail-stalwart --timeout=600s
   ```
3. Trigger Stalwart's FTS reindex for the shard (rebuilds the search
   index from stored messages). Watch `kmail_search_cutover_*` and
   Stalwart logs until the backlog drains.
4. Re-probe, then set the shard back to `active` (PUT, as in §2).

## 4. If the shard cannot be recovered quickly — evacuate

If recovery will exceed the error-budget window, migrate tenants to a
healthy shard with spare capacity (see `tenant-migration.md`). Bulk loop:

```bash
# Tenants currently on the dead shard:
TENANTS=$(curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards/<dead-shard-id>" | jq -r '.tenants[]?')
# (GET /shards/{id} returns {"shard":{...},"tenants":[...]}.)
# Rebalance is POSTed to the TARGET shard ({id} = destination); the
# source is in the body as from_shard_id (see shard_handlers.go).
for t in $TENANTS; do
  curl -s -X POST -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
    -H 'Content-Type: application/json' \
    "$API/api/v1/admin/shards/<healthy-shard-id>/rebalance" \
    -d "{\"from_shard_id\": \"<dead-shard-id>\", \"tenant_id\": \"$t\"}"
done
```

## 5. Post-incident

- Set the shard back to `active` only after `…/shards/health` shows it
  `active` and healthy for 5 min.
- Confirm the auto-provisioner didn't spin an extra shard during the
  outage; if it did and it's now idle, decommission it.
- File the incident (see `incident-response.md` §Postmortem).
- If the failure was an AZ loss, confirm `topologySpreadConstraints`
  actually spread replicas (`kubectl get pods -o wide` across zones).
