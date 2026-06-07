# KMail operator documentation

Operational guides for running KMail in production: deploying the
control plane and mail core, planning capacity, upgrading safely,
backing up and restoring data, and monitoring the running system.

These guides are written for SREs/operators. For local development see
[`../DEVELOPMENT.md`](../DEVELOPMENT.md); for the architecture rationale
see [`../ARCHITECTURE.md`](../ARCHITECTURE.md).

## Guides

| Guide | What it covers |
| ----- | -------------- |
| [Deployment](./deployment.md) | Installing the Helm chart (BFF + worker), and deploying the Stalwart mail shards on long-lived hosts. |
| [Capacity planning](./capacity-planning.md) | Sizing replicas, HPA, resource requests/limits, shard count, and outbound IP pools. |
| [Upgrade](./upgrade.md) | `helm upgrade` flow, image bumps, the Phase A migration gates, and the mTLS scaling rule. |
| [Backup & restore](./backup-restore.md) | What to back up (Postgres, blob fabric, config/secrets), and how to restore. |
| [Monitoring & alerting](./monitoring.md) | Prometheus scrape targets, the shipped alert rules, Grafana dashboards, and Loki log shipping. |

## Production topology at a glance

```
                ┌── kmail-api (Go BFF / JMAP proxy) ──┐   Kubernetes
   clients ──►  │   Deployment + HPA + PDB            │   (Helm chart:
                │   kmail-worker (background jobs)     │    deploy/helm/kmail)
                └──────────────┬──────────────────────┘
                               │  /jmap/*  (per-tenant shard routing)
                               ▼
                Stalwart mail shards on long-lived VMs / bare metal
                (deploy/stalwart/, deploy/terraform/shard/)
                               │
                               ▼
        shared Postgres · zk-object-fabric (S3) · Valkey · Meilisearch/OpenSearch
```

> **Important placement constraint.** The Go control plane
> (`kmail-api`, `kmail-worker`) is stateless and runs on Kubernetes via
> the Helm chart. **Stalwart should run on long-lived hosts (VMs or
> bare metal), not on ephemeral Kubernetes pods**, because outbound
> SMTP IP reputation must survive node rescheduling — see
> [`../ARCHITECTURE.md`](../ARCHITECTURE.md) §11 and
> [`../../deploy/stalwart/README.md`](../../deploy/stalwart/README.md).
> The chart's Stalwart `StatefulSet` exists for dev/staging clusters
> and single-node installs; the production HA topology is the VM-based
> shard model documented in the deployment guide.

## Source-of-truth references

- Helm chart: [`../../deploy/helm/kmail/`](../../deploy/helm/kmail/)
  (defaults in `values.yaml`, multi-region overlay in
  `values-multiregion.yaml`, post-install notes in `templates/NOTES.txt`).
- Stalwart HA shard guide: [`../../deploy/stalwart/README.md`](../../deploy/stalwart/README.md)
  and per-node template `deploy/stalwart/ha-config.json`.
- Shard provisioning Terraform: [`../../deploy/terraform/shard/`](../../deploy/terraform/shard/).
- Monitoring configs: `deploy/prometheus/`, `deploy/grafana/`,
  `deploy/loki/`, `deploy/promtail/`.
