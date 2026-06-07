# Backup & restore procedures

What to back up, how often, and how to restore each KMail data store.
The recovery targets here match the program documented in
[`../compliance/SECURITY_OVERVIEW.md`](../compliance/SECURITY_OVERVIEW.md)
§8 and the SOC 2 control mapping.

## What holds state

| Store | Contains | Backup posture | Rebuildable? |
| ----- | -------- | -------------- | ------------ |
| **PostgreSQL** | Control-plane metadata **and** Stalwart's mail data store | Daily full + WAL archived every 5 min; PITR window 35 days; monthly snapshot retained 1 year | No — primary system of record |
| **zk-object-fabric (S3)** | Mail bodies / attachment blobs | Multi-AZ replicated, object versioning enabled | No |
| **Config & secrets** | Helm values, the referenced Secret, cert-manager Issuer config | Version-controlled values + secret store backup | Partially |
| **Valkey** | Rate limits, undo-send buffer, sessions, push queue (short-TTL) | Not backed up — ephemeral by design | Yes (regenerates) |
| **Meilisearch / OpenSearch** | Search indices | Not backed up — derived data | Yes (reindex from mail) |

> Both the control-plane database and Stalwart's data store live in
> PostgreSQL (Stalwart uses a dedicated `stalwart` database). A
> consistent PostgreSQL backup therefore captures **both** the
> control-plane state and the mail store.

## PostgreSQL

### Backup

Use managed PITR (the documented target is daily full + 5-minute WAL
archiving, 35-day PITR window, monthly snapshots kept for a year). If
you run Postgres yourself, configure continuous WAL archiving
(`archive_command`) plus periodic base backups (e.g. `pgBackRest` or
`wal-g`). A logical dump is a useful secondary for a single database:

```bash
pg_dump --format=custom --no-owner "$KMAIL_DATABASE_URL" -f kmail-control.dump
# Stalwart's data store (separate database on the same server):
pg_dump --format=custom --no-owner "$STALWART_DATABASE_URL" -f stalwart.dump
```

### Restore

1. **Stop writers** to the target database (scale `kmail-api` and
   `kmail-worker` to 0, and the affected Stalwart shard nodes).
2. Restore the cluster to the desired point in time (PITR) or load the
   dump into a fresh database:
   ```bash
   pg_restore --clean --if-exists --no-owner -d "$KMAIL_DATABASE_URL" kmail-control.dump
   ```
3. Run pending migrations to reconcile schema:
   `./scripts/migrate.sh status && ./scripts/migrate.sh up`.
4. Bring the control plane back up and verify `/healthz` and
   `/debug/config`.
5. After any restore, verify the audit chain integrity (the log is
   hash-linked; see
   [`../compliance/incident-response.md`](../compliance/incident-response.md)).

## zk-object-fabric (mail blobs)

Mail bodies and attachments live in the S3-compatible object fabric,
which is multi-AZ replicated with object versioning. Recovery options:

- **Accidental object deletion/overwrite**: restore the prior object
  version (versioning is enabled).
- **Bucket/region loss**: fail over to the replicated copy.

Restore blob storage and the control-plane database to the **same point
in time** — blob references in Postgres must resolve to objects that
exist. If a Postgres PITR restore predates some blobs, those blobs are
simply unreferenced (harmless); if it postdates a blob deletion,
restore the blob versions to match.

## Config & secrets

- Keep Helm `values.yaml` overrides in version control.
- Back up the referenced Kubernetes Secret (`secret.existingName`) in
  your secret manager, not just in-cluster.
- Record the cert-manager Issuer used for BFF↔Stalwart mTLS so the
  trust chain can be re-established (see
  [deployment](./deployment.md#4-bffstalwart-mtls-recommended)).

## Derived stores (no restore needed)

- **Valkey** — ephemeral; on loss, rate limits and undo-send buffers
  reset and sessions re-authenticate. No restore step.
- **Meilisearch / OpenSearch** — rebuild the index from mail data
  rather than restoring a snapshot (the search-cutover worker manages
  reindexing).

## Disaster-recovery drills

Run periodic restore drills (the program targets quarterly) that
exercise a Postgres PITR restore + blob version restore + audit-chain
verification end to end, and record the achieved RTO/RPO.
