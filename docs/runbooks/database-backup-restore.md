# Runbook: Database backup & restore

**Goal:** take and verify Postgres backups, and restore the
control-plane or a per-shard database after corruption/loss.

KMail has two classes of Postgres:
- **Control-plane DB** — tenants, shard registry, billing, sessions.
  Losing it loses routing for everyone.
- **Per-shard DB** — Stalwart's mailbox metadata for that shard's
  tenants.

Both are provisioned by `deploy/terraform/modules/postgres` with managed
automated backups (`backup_retention_days`, default 14). This runbook
covers verifying those, plus logical dumps for portability.

## 1. Automated (managed) backups — verify

```bash
# Confirm the managed instance has recent automated snapshots and PITR.
# (provider CLI; example shape — substitute your provider)
#   <cloud> db describe <instance> --query 'backup.{retention,latest}'
```

- Confirm `backup_retention_days` matches policy (Terraform var).
- Confirm point-in-time-recovery (PITR) window covers your RPO.
- Record the latest restorable timestamp in the incident doc.

## 2. On-demand logical dump (portable)

Run from a maintenance pod/jump host with network access to the DB.

```bash
# Control-plane DB
pg_dump --no-owner --format=custom "$KMAIL_DATABASE_URL" \
  -f kmail-controlplane-$(date +%Y%m%dT%H%M%SZ).dump

# A shard DB (DSN from `terraform -chdir=deploy/terraform/shard output -json`)
pg_dump --no-owner --format=custom "$SHARD_DSN" \
  -f kmail-<shard>-$(date +%Y%m%dT%H%M%SZ).dump
```

Upload dumps to the backup bucket (versioned, SSE-encrypted — see
`deploy/terraform/modules/object-store`). Never store dumps unencrypted
or on an operator laptop.

## 3. Verify a backup is restorable (do this routinely, not just in a crisis)

```bash
# Restore into a throwaway database and run a sanity query.
createdb kmail_verify
pg_restore --no-owner -d "postgres://.../kmail_verify" kmail-controlplane-*.dump
psql "postgres://.../kmail_verify" -c "select count(*) from tenants;"
psql "postgres://.../kmail_verify" -c "select id, healthy, current_mailboxes, max_mailboxes from stalwart_shards;"
dropdb kmail_verify
```

A backup you have never restored is not a backup. Schedule this monthly.

## 4. Restore (disaster)

> Restoring the control-plane DB is a full-control-plane operation.
> Declare an incident (`incident-response.md`) and freeze writes first.

### 4a. Managed PITR (preferred — lowest RPO)

1. Freeze writes: scale the BFF to 0 so no new assignments occur
   (`kubectl -n kmail scale deploy/kmail-api --replicas=0`).
2. Trigger the provider's PITR to the chosen timestamp into a NEW
   instance.
3. Repoint `KMAIL_DATABASE_URL` (control-plane) or the shard's DSN at the
   restored endpoint (update the Secret / shard registry).
4. Bring the BFF back up (`--replicas=3`) and verify (§5).

### 4b. Logical restore (from a dump)

1. Freeze writes (as above).
2. Provision a clean DB and `pg_restore` the chosen dump.
3. Repoint the connection string; restart consumers.

## 5. Post-restore verification

```bash
kubectl -n kmail scale deploy/kmail-api --replicas=3
kubectl -n kmail rollout status deploy/kmail-api --timeout=180s

# Routing intact?
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" \
  "$API/api/v1/admin/shards/health" | jq .
```

- Spot-check a few tenants can log in and load mail.
- Reconcile shard `current_mailboxes` if the restore predates recent
  signups (`PUT /api/v1/admin/shards/{id}`).
- Note the data-loss window (now − restore point) in the postmortem.

## RPO / RTO targets

| DB | RPO | RTO |
| --- | --- | --- |
| Control-plane | ≤ 5 min (PITR) | ≤ 30 min |
| Per-shard | ≤ 15 min | ≤ 60 min (or evacuate tenants, see failover) |
