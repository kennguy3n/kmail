# Runbook: Tenant migration between shards

**Goal:** move one or more tenants from a source shard to a target
shard (rebalancing a hot shard, evacuating a failing one, or
consolidating). Uses the control plane's rebalance API so routing stays
consistent.

**Triggers:**
- A shard is at/near `max_mailboxes` while others have headroom.
- Shard evacuation during failover (`shard-failover.md` §4).
- Cost/locality consolidation.

## 0. How migration works

`POST /api/v1/admin/shards/{id}/rebalance` calls
`ShardService.RebalanceShard(fromShardID, toShardID, tenantID)` which
re-points the tenant→shard assignment and invalidates the resolver
cache. **The path `{id}` is the DESTINATION shard; the source is sent in
the body as `from_shard_id`** (see `internal/tenant/shard_handlers.go`).
Mail data itself (mailboxes + blobs) must be COPIED to the target
shard's Stalwart before the cutover, because the assignment only changes
*where the BFF looks*. Order matters: copy first, cut over second.

## 1. Pre-flight (5 min)

```bash
# Capacity check — target must have room. List returns {"shards":[...]}.
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards" | jq '.shards[] | {id, current_mailboxes, max_mailboxes, status}'
```

- Confirm `target.current_mailboxes + moved <= target.max_mailboxes`.
- Confirm BOTH shards are `status: active` (source must be readable to
  copy; for a dead source, use the restore path in
  `database-backup-restore.md`).
- Announce a maintenance window for the tenant if a brief read-only
  period is required.

## 2. Copy mailbox data to the target shard

1. Put the tenant's mailboxes into a quiesced state (optional but
   reduces delta): pause inbound delivery for the domain or accept a
   small re-sync window.
2. Copy Stalwart account data from source to target shard for the
   tenant. Blobs are already shared in S3, so this copies metadata +
   IMAP/JMAP account state. Use `imapsync` (the repo wires
   `KMAIL_IMAPSYNC_BIN`) or Stalwart's account export/import between the
   two shard endpoints.
3. Verify message counts match on the target before cutover.

## 3. Cut over the assignment

```bash
# POST to the DESTINATION shard; name the source in the body.
curl -s -X POST -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  "$API/api/v1/admin/shards/<to-shard-id>/rebalance" \
  -d '{"from_shard_id": "<from-shard-id>", "tenant_id": "<tenant-id>"}'
```

This updates the assignment and invalidates the cache immediately;
subsequent JMAP requests resolve to the target shard.

## 4. Verify

```bash
# Resolver now points the tenant at the target shard.
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards" | jq '.shards[] | {id, current_mailboxes}'
```

- Send a test message to a tenant mailbox and confirm it lands on the
  target shard.
- Watch `kmail_jmap_proxy_duration_seconds` for the tenant's traffic —
  errors here mean the resolver/target mismatch; re-check the copy.

## 5. Clean up

- Once verified for 24h, delete the tenant's stale account data from the
  **source** shard to reclaim space (do NOT delete shared S3 blobs —
  they may be referenced by other tenants).
- Update `current_mailboxes` is automatic; if it drifts, reconcile via
  `PUT /api/v1/admin/shards/{id}`.

## Rollback

If verification fails before cleanup, re-run the rebalance in reverse:
POST to the ORIGINAL source shard with `from_shard_id` set to the
target, same `tenant_id`. Because source data is still intact (you
haven't cleaned up yet), this is a safe instant rollback.
