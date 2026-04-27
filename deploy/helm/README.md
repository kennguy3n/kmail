# KMail Helm chart

This chart packages the production deployment topology of KMail
on Kubernetes:

- `kmail-api` — Go BFF, deployed as a horizontally scaled
  Deployment with a Service, optional Ingress, HPA, and PDB.
- `stalwart` — mail core, deployed as a StatefulSet with stable
  per-pod hostnames via `spec.subdomain` + a headless Service.
  Mail nodes are explicitly **not** autoscaled (see
  `do-not-do.md`); resize manually.

The chart is deliberately scoped to the kmail-api + stalwart
pair. Stateful dependencies (Postgres, Valkey, zk-object-fabric,
Meilisearch / OpenSearch) live in upstream charts your platform
team owns; this chart only references their endpoints via the
`secret.data.*` values.

## Installation

```bash
# Lint locally
make helm-lint

# Render templates
helm template kmail ./deploy/helm/kmail --debug

# Install
helm install kmail ./deploy/helm/kmail \
  --namespace kmail \
  --create-namespace \
  --set image.tag=phase-7
```

For production, bring your own `Secret` so credentials live in
your secret store rather than the chart values:

```bash
kubectl create secret generic kmail-secrets \
  --from-literal=KMAIL_DATABASE_URL=... \
  --from-literal=KMAIL_KCHAT_OIDC_CLIENT_SECRET=... \
  --from-literal=KMAIL_STRIPE_SECRET_KEY=...

helm install kmail ./deploy/helm/kmail \
  --set secret.create=false \
  --set secret.existingName=kmail-secrets
```

## Values surface

`values.yaml` is the canonical reference. Headline knobs:

- `image.repository` / `image.tag` — kmail-api container image.
- `kmailApi.replicaCount`, `kmailApi.hpa.*` — autoscaler bounds.
- `kmailApi.config.*` — every `internal/config` env var. Update
  these alongside the Go config when adding new env vars.
- `secret.data.*` — credentials baked into a chart-managed Secret
  (only used when `secret.create=true`).
- `stalwart.replicaCount` / `stalwart.storage.*` — Stalwart
  StatefulSet sizing. Stalwart instances are not autoscaled.

## Why Stalwart isn't on an HPA

Stalwart owns durable mailbox state. Auto-pruning a pod (the HPA
shrinking from 3 → 2) would orphan the in-flight blob lookups and
force a re-shard, which is the kind of unplanned move the
do-not-do list calls out. Resize the StatefulSet manually after
draining tenants off the doomed pod via the BFF's shard rebalance
endpoint.
