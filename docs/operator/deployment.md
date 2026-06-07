# Deployment guide

This guide deploys KMail to production: the stateless Go control plane
(`kmail-api` + `kmail-worker`) on Kubernetes via the Helm chart, and
the Stalwart mail shards on long-lived hosts.

> Read the [placement constraint](./README.md#production-topology-at-a-glance)
> first: the control plane runs on Kubernetes; **Stalwart runs on
> VMs/bare metal**, not ephemeral pods.

## Prerequisites

- A Kubernetes cluster and Helm 3.x (`make helm-lint` validates the
  chart locally).
- An ingress controller (the chart defaults to `className: nginx`).
- Shared backing services reachable from the cluster:
  PostgreSQL 16, Valkey 8, a zk-object-fabric (S3-compatible) endpoint,
  and Meilisearch (or OpenSearch after cutover).
- An OIDC issuer (the KChat identity provider) — **required** in
  staging/production (see [Upgrade](./upgrade.md#phase-a-migration-gates)).
- A container image for the control plane. The
  [`Dockerfile`](../../Dockerfile) builds every `cmd/*` binary into one
  image; the entrypoint selects the process (`kmail-api` by default,
  `kmail-worker` for the worker Deployment).

## 1. Configure values

All `internal/config` env vars are exposed under `kmailApi.config.*` in
[`values.yaml`](../../deploy/helm/kmail/values.yaml). Secrets
(`KMAIL_DATABASE_URL`, OIDC client secret, S3 keys, Stripe key) come
from a referenced Secret. For production, do **not** let the chart
create the Secret — provide your own:

```yaml
secret:
  create: false
  existingName: kmail-prod-secrets

kmailApi:
  config:
    KMAIL_KCHAT_OIDC_ISSUER: "https://kchat.example.com"
    KMAIL_KCHAT_OIDC_AUDIENCE: "kmail-prod"
    KMAIL_STALWART_URL: "http://<stalwart-shard-lb>:8080"

image:
  repository: ghcr.io/kennguy3n/kmail-api
  tag: <release-tag>
```

The empty `KMAIL_KCHAT_OIDC_ISSUER` default is intentional — leaving it
empty makes the BFF fail closed at boot rather than trusting unverified
JWTs.

## 2. Install the control plane

```bash
helm install kmail ./deploy/helm/kmail \
  -f values.yaml -f my-overrides.yaml \
  --namespace kmail --create-namespace
```

The chart deploys:

- **`kmail-api`** — the BFF/JMAP proxy, a horizontally scaled
  `Deployment` with an `HPA` (3–12 replicas, 70% CPU target by default)
  and a `PodDisruptionBudget`.
- **`kmail-worker`** — the background worker process (calendar
  reminders, undo/scheduled/snooze dispatch, billing scan, search
  cutover, deliverability alerts, shard-health, retention, export
  fan-out, webhooks). It exposes only `/healthz`, `/readyz`, and
  `/metrics` on port `8090`; no Service or Ingress.

The single listen port for the BFF is driven by
`kmailApi.service.targetPort` (default `8080`) — the container port,
`KMAIL_API_ADDR`, and probe targets are all derived from it, so change
it in exactly one place.

### Verify

Follow the post-install checklist printed by the chart
([`templates/NOTES.txt`](../../deploy/helm/kmail/templates/NOTES.txt)):

```bash
kubectl -n kmail rollout status deployment/kmail-api --timeout=180s
kubectl -n kmail logs -l app.kubernetes.io/component=api -f --tail=200
# In-use config with secrets redacted:
kubectl -n kmail exec deployment/kmail-api -- wget -qO- http://localhost:8080/debug/config
```

## 3. Deploy the Stalwart mail shards

Production Stalwart runs as one or more **shards** on long-lived hosts,
each shard being 2+ nodes (one primary, warm secondaries) behind a load
balancer. A tenant pins to a shard via `tenant_shard_assignments`; the
BFF JMAP proxy resolves the primary per request and fails over to
backups using `shard_failover_config`.

Follow [`deploy/stalwart/README.md`](../../deploy/stalwart/README.md):

1. Provision shard infrastructure with the Terraform module in
   [`deploy/terraform/shard/`](../../deploy/terraform/shard/) (it is
   provider-agnostic; wire your cloud's child modules into it).
2. Render each node from the `deploy/stalwart/ha-config.json` template,
   replacing the `REPLACE_*` tokens (node ID, PTR record, stable
   outbound IP, shard name). Shared values (Postgres, Meilisearch,
   Valkey) come from the per-node env vars listed in that guide.
3. Front each shard's JMAP listener with a TCP/HTTP load balancer that
   health-checks `/healthz`, terminates (or passes through) TLS, and
   accepts the JMAP `Connection: upgrade` for EventSource push.
4. Point `KMAIL_STALWART_URL` (or the per-shard records the proxy
   reads) at the shard load balancers.

For outbound IP reputation requirements (one stable IP per pool,
forward-confirmed reverse DNS, warmup), see the
[capacity planning guide](./capacity-planning.md#outbound-ip-pools).

## 4. BFF→Stalwart mTLS (recommended)

In production the BFF authenticates to Stalwart with a client
certificate instead of a trusted-network header. Enable it with
cert-manager:

```yaml
mtls:
  enabled: true
  issuerRef:
    name: internal-pki
    kind: ClusterIssuer
```

When enabled, the chart renders client/server `Certificate` resources
and overrides `KMAIL_STALWART_URL` to the HTTPS listener (port 8443) so
SNI matches a SAN — you do not edit the ConfigMap URL yourself. The
chosen Issuer **must** emit `ca.crt`; if it doesn't, the BFF fails fast
with a missing-trust-anchor error. See
[`../DEVELOPMENT.md`](../DEVELOPMENT.md) "cert-manager Issuer must emit
`ca.crt`" for resolutions.

> When mTLS is on, the Stalwart server cert SAN list is generated from
> `stalwart.replicaCount` at render time. **Always** scale Stalwart via
> `helm upgrade --set stalwart.replicaCount=N`, never raw
> `kubectl scale statefulset` — see [Upgrade](./upgrade.md#scaling-stalwart-with-mtls).

## 5. Multi-region (optional)

Layer the multi-region overlay on top of the base values:

```bash
helm upgrade kmail ./deploy/helm/kmail \
  -f values.yaml -f deploy/helm/kmail/values-multiregion.yaml \
  --set multiregion.region=us-west-2
```

This wires region-aware ingress annotations, the `KMAIL_REGION` env,
topology spread constraints, and a cross-region Valkey peer list. With
`multiregion.enabled=false` (default) installs behave as single-region.
