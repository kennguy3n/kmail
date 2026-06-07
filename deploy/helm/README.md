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

## Production hardening (5,000 tenants / 10 shards)

The chart ships several production features **disabled by default** so a
vanilla `helm install` still works on a kind/minikube/dev cluster. Turn
them on deliberately once the cluster prerequisites are in place.

| Feature | Value | Default | Cluster prerequisite |
| --- | --- | --- | --- |
| Cross-AZ pod spread | `*.topologySpreadConstraints` | **on** (`ScheduleAnyway`) | none — safe everywhere |
| NetworkPolicy segmentation | `networkPolicy.enabled` | off | a CNI that enforces NetworkPolicy (Calico/Cilium) |
| Prometheus ServiceMonitor | `serviceMonitor.enabled` | off | Prometheus Operator CRDs (kube-prometheus-stack) |
| PrometheusRule (alerts + SLO burn-rate) | `prometheusRule.enabled` | off | Prometheus Operator CRDs |
| Grafana dashboard auto-import | `grafanaDashboards.enabled` | off | Grafana dashboard sidecar |
| BFF→Stalwart mTLS | `mtls.enabled` | off | cert-manager (+ Reloader) |

Render the full surface without a cluster:

```bash
make helm-template   # templates with every feature above enabled
```

### Resource-limits tuning guide

The defaults in `values.yaml` are sized for the **5K-tenant / 10-shard**
target (≈500 mailboxes per shard). They follow the Kubernetes guidance
of setting `requests` well below `limits` so pods bin-pack efficiently
but can burst:

| Workload | requests (cpu/mem) | limits (cpu/mem) | Scaling lever |
| --- | --- | --- | --- |
| `kmail-api` (BFF) | 250m / 256Mi | 1000m / 1Gi | HPA 3→12 @ 70% CPU |
| `kmail-worker` | 100m / 128Mi | 500m / 512Mi | manual `replicaCount` |
| `stalwart` (per shard) | 500m / 1Gi | 2000m / 4Gi | manual `replicaCount` (NOT autoscaled) |

Sizing rules of thumb at the 5K-tenant target:

- **API**: a BFF replica sustains ~300–400 req/s before the p99 latency
  SLO degrades. With HPA min 3 / max 12 and the 70% CPU target the chart
  handles a 10x burst over steady-state before saturating. Raise
  `kmailApi.hpa.maxReplicas` (and the PDB) if your steady-state RPS
  pushes the average replica past ~60% CPU.
- **Worker**: a single worker drains the per-feature job queues for the
  whole fleet. Scale `kmailWorker.replicaCount` up (and rely on the
  topology spread) only if `kmail_worker_*` queue/latency metrics show
  sustained backlog — the jobs are idempotent and safe to run N-way.
- **Stalwart**: memory is the binding constraint (FTS + page cache).
  Budget ~1Gi per ~150 active mailboxes on a shard before raising the
  limit. Stalwart is **not** autoscaled; add shards (more
  StatefulSet-backed cells via `deploy/terraform/shard`) rather than
  growing a single shard unbounded.

See `docs/runbooks/capacity-planning.md` for the full per-shard capacity
model and the tenant-count → shard-count math.

### Stalwart StatefulSet volume sizing

Each Stalwart replica is one shard and gets its **own** PVC
(`volumeClaimTemplates`) sized by `stalwart.storage.size` (default
`50Gi`). Message bodies/attachments live in the S3 blob store, **not**
on this volume — the PVC holds Stalwart's metadata store, FTS index,
and queue spool (~5–10% of raw mail volume). For ~500 mailboxes/shard ×
~20k msgs/mailbox that is ~20Gi of index+spool; `50Gi` leaves ~2x
headroom for FTS rebuilds. Use an SSD-backed, topology-aware
`storageClass` with `allowVolumeExpansion: true` so volumes can be
grown online. Full model in `docs/runbooks/capacity-planning.md`.

### NetworkPolicy posture

`networkPolicy.enabled=true` renders a default-deny baseline plus
per-component allow rules (api / worker / stalwart). Ingress is always
restricted; **egress** is only restricted when
`networkPolicy.restrictEgress=true` (off by default — over-tight egress
is the most common self-inflicted outage). Before enabling, confirm the
`networkPolicy.ingressControllerNamespaceSelector` and
`networkPolicy.monitoringNamespaceSelector` match your cluster's
namespace labels.

### Monitoring

- `serviceMonitor.enabled=true` wires the api + worker `/metrics` into a
  Prometheus Operator. Set `serviceMonitor.labels.release` to your
  Prometheus release so the operator selects it.
- `prometheusRule.enabled=true` ships the alert family from
  `deploy/prometheus/alerts.yml` **plus** multi-window multi-burn-rate
  SLO error-budget alerts (target `prometheusRule.slo.availabilityTarget`,
  default 99.9%).
- `grafanaDashboards.enabled=true` embeds the dashboards from
  `deploy/grafana/dashboards/` as sidecar-labeled ConfigMaps. The chart
  keeps a mirror under `deploy/helm/kmail/dashboards/`; re-sync it after
  editing a dashboard with `make helm-sync-dashboards`.

## Why Stalwart isn't on an HPA

Stalwart owns durable mailbox state. Auto-pruning a pod (the HPA
shrinking from 3 → 2) would orphan the in-flight blob lookups and
force a re-shard, which is the kind of unplanned move the
do-not-do list calls out. Resize the StatefulSet manually after
draining tenants off the doomed pod via the BFF's shard rebalance
endpoint.
