# Upgrade guide

How to roll a new KMail release safely. The general flow is: apply
database migrations, then roll the control plane, then (separately, and
rarely) the Stalwart shards.

## 1. Read the release migration notes

Before upgrading, check the chart's
[`templates/NOTES.txt`](../../deploy/helm/kmail/templates/NOTES.txt) and
the repo `README.md` upgrade notes for behavior changes that gate the
release. The notable ones are in [§ Phase A migration gates](#phase-a-migration-gates)
below.

## 2. Apply database migrations

Migrations live in [`migrations/`](../../migrations/) and are applied by
the `kmail-migrate` runner (wrapped by
[`scripts/migrate.sh`](../../scripts/migrate.sh) / `make migrate`). The
runner takes a Postgres **advisory lock**, so two concurrent rolling
deploys cannot apply the same migration twice — it is safe for
zero-downtime rollouts.

```bash
DATABASE_URL=postgres://... ./scripts/migrate.sh status   # see pending
DATABASE_URL=postgres://... ./scripts/migrate.sh up        # apply all pending
# Roll back the last migration (requires a NNN_*.down.sql companion):
DATABASE_URL=postgres://... ./scripts/migrate.sh down 1
```

Apply migrations **before** rolling pods that expect the new schema.
Migrations are additive/backwards-compatible where possible so the old
and new code can run simultaneously during a rolling deploy.

## 3. Roll the control plane

Bump the image tag and upgrade:

```bash
helm upgrade kmail ./deploy/helm/kmail \
  --reuse-values \
  --set image.tag=<new-release-tag>

kubectl -n kmail rollout status deployment/kmail-api --timeout=180s
kubectl -n kmail rollout status deployment/kmail-worker --timeout=180s
```

The `kmail-worker` Deployment is rolled the same way (it shares the API
ConfigMap and Secret). If a referenced Secret/ConfigMap rotated and
Reloader is not installed, force a restart:

```bash
kubectl -n kmail rollout restart deployment/kmail-api
```

## 4. Roll the Stalwart shards (when needed)

Stalwart upgrades are infrequent and node-by-node to preserve IP
reputation and availability. Drain a shard's primary, upgrade it, let
the shard-health worker confirm it healthy, then move to the next node.
To drain a primary for maintenance the BFF fails traffic over to the
next-priority backup automatically:

```bash
# Mark the primary unhealthy; the health worker resets the flag once
# the host returns. Traffic flows to the next backup meanwhile.
curl -X PUT https://api.../api/v1/admin/shards/{shard_id}/health \
  -H "Authorization: Bearer $ADMIN_TOKEN" -d '{"healthy":false}'
```

See [`deploy/stalwart/README.md`](../../deploy/stalwart/README.md)
"Disaster failover" for details.

## Phase A migration gates

Two Phase A changes affect existing staging/production installs.
Confirm both before flipping live traffic.

### OIDC fail-closed

The BFF now **refuses to boot** in any non-development environment when
`KMAIL_KCHAT_OIDC_ISSUER` (or bare `KCHAT_OIDC_ISSUER`) is empty.
Previously an empty issuer silently degraded auth to an unverified-JWT
fallback — a security regression. Set the issuer in your values:

```yaml
kmailApi:
  config:
    KMAIL_KCHAT_OIDC_ISSUER: "https://kchat.example.com"
    KMAIL_KCHAT_OIDC_AUDIENCE: "kmail-prod"
```

Verify the JWKS endpoint is reachable from the BFF pods, then re-roll.
There is no opt-out in staging/production — the dev bypass and
unverified fallback are hard-locked behind `KMAIL_ENV=development`.

### Worker process decomposition

Background workers were split out of `kmail-api` into the dedicated
`kmail-worker` process. The Helm chart already deploys `kmail-worker`,
so chart users get this automatically. **Non-Helm deployments** must
either deploy `kmail-worker` alongside `kmail-api`, or set
`KMAIL_DISABLE_WORKERS=false` on `kmail-api` to keep running them
in-process. (See `README.md` upgrade note.)

## Scaling Stalwart with mTLS

When `mtls.enabled=true`, the Stalwart server certificate SAN list is
generated from `stalwart.replicaCount` at template-render time. Scaling
the StatefulSet **without** a matching `helm upgrade` leaves new
replicas presenting a cert whose SANs exclude their pod DNS names, and
BFF→Stalwart handshakes fail with `x509: certificate is valid for X,
not Y`. Always scale via Helm:

```bash
# WRONG — stale SAN list, new pods fail the TLS handshake:
kubectl scale statefulset/kmail-stalwart --replicas=4

# RIGHT — re-renders the Certificate, cert-manager reissues with all
# pod DNS names, Reloader restarts pods to pick up the new cert:
helm upgrade kmail ./deploy/helm/kmail --reuse-values \
  --set stalwart.replicaCount=4
```

## Rollback

- **Control plane**: `helm rollback kmail <revision>` (or re-deploy the
  previous `image.tag`). The control plane is stateless.
- **Database**: roll back the last migration(s) with
  `./scripts/migrate.sh down N` only if the new code is also rolled
  back and the `.down.sql` companion exists. Prefer forward fixes for
  data-bearing migrations.
