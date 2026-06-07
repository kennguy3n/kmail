# Runbook: Stalwart upgrade (fleet rollout)

**Goal:** move the production Stalwart pin from one version to another
across all ~10 shards safely.

> The **canonical** version matrix, the v0.16.0 ↔ v1.0.0 breaking
> changes, and the BFF compatibility shim (`internal/jmap/compat.go`)
> are documented in [`../STALWART_UPGRADE.md`](../STALWART_UPGRADE.md).
> **Read that first.** This runbook is the operational rollout procedure
> that wraps it.

## 0. Preconditions

- `scripts/test-stalwart-upgrade.sh` passes against a staging blue/green
  pair.
- The target image tag is published and pinned in the Helm values
  (`stalwart.image.tag`).
- A 24h staging soak on the target version is complete.
- You have a current per-shard backup (`database-backup-restore.md`) and
  a verified restore.

## 1. Roll one canary shard first

Stalwart is a StatefulSet; pods update on the `OnDelete` /
`RollingUpdate` strategy in the chart. Upgrade a SINGLE low-traffic
shard first.

```bash
# Bump the image for the canary shard only (per-shard values overlay).
helm upgrade kmail ./deploy/helm/kmail \
  --namespace kmail --reuse-values \
  --set stalwart.image.tag=<new-version> \
  --set stalwart.updateStrategy.partition=<n>     # only ordinals >= n update

kubectl -n kmail rollout status statefulset/kmail-stalwart --timeout=600s
```

Validate the canary:
- `GET /api/v1/admin/shards/health` → canary `healthy:true`.
- The compat shim detects the new version (BFF logs `stalwartVersion`).
- Send/receive a test message through a canary-shard tenant.
- Watch `kmail_jmap_proxy_duration_seconds` and error rate for the
  canary for at least one soak window (≥ 1h).

## 2. Progressive fleet rollout

Lower the `updateStrategy.partition` in steps so ordinals update a few at a time,
pausing for a soak between steps:

```bash
for p in 8 6 4 2 0; do
  helm upgrade kmail ./deploy/helm/kmail --namespace kmail --reuse-values \
    --set stalwart.image.tag=<new-version> --set stalwart.updateStrategy.partition=$p
  kubectl -n kmail rollout status statefulset/kmail-stalwart --timeout=600s
  # SOAK: watch dashboards + shard health before the next step.
done
```

Each pod keeps its PVC across the restart, so no reindex is needed for a
normal version bump. (A bump that changes the on-disk index format will
say so in the release notes → schedule a reindex and extra headroom.)

## 3. Drop the compatibility shim (later release)

Once **every** shard reports the new version and has soaked, the BFF can
stop emulating the old shape. This is a SEPARATE BFF release — see
`docs/STALWART_UPGRADE.md` for when the shim default flips. Do not remove
the shim in the same change as the data-plane upgrade.

## 4. Rollback

If a shard misbehaves on the new version:

```bash
helm upgrade kmail ./deploy/helm/kmail --namespace kmail --reuse-values \
  --set stalwart.image.tag=<previous-version> \
  --set stalwart.updateStrategy.partition=<n>
kubectl -n kmail rollout status statefulset/kmail-stalwart --timeout=600s
```

The compat shim falls back to the v0.16.0 shape for any shard whose
version it can't parse, so a mixed-version fleet stays serving during
rollback. If the new version migrated the on-disk format, rollback
requires restoring the shard PVC/DB from backup (`database-backup-restore.md`).
