# 0006 — Running Stalwart on long-lived hosts, not Kubernetes pods

- **Status**: Accepted
- **Related**: [`../../deploy/stalwart/README.md`](../../deploy/stalwart/README.md), [`../ARCHITECTURE.md`](../ARCHITECTURE.md) §11, [`../../deploy/terraform/shard/`](../../deploy/terraform/shard/)

## Context

The stateless control plane runs happily on Kubernetes. Stalwart is
different: it sends outbound SMTP, and deliverability depends on the
**reputation of stable outbound IPs**. IPs that churn as pods are
rescheduled cannot build or keep reputation, and large receivers
downrank or block mail from cold/rotating IPs. Stalwart is also
stateful in a way that benefits from stable per-node identity and
local disk.

## Decision

Run **Stalwart on long-lived hosts (VMs or bare metal)**, organised
into **shards**. Each shard is a Postgres logical group of tenants with
2+ nodes (one primary, warm secondaries) behind a load balancer.
Tenants pin to a shard via `tenant_shard_assignments`; the BFF JMAP
proxy resolves the primary per request and fails over to backups by
`shard_failover_config.priority`, circuit-breaking a host after
`KMAIL_PROXY_CIRCUIT_THRESHOLD` consecutive failures. Each shard owns
stable outbound IPs (one per pool) with forward-confirmed reverse DNS
and a warmup ramp.

The Helm chart's Stalwart `StatefulSet` is retained for **dev/staging
and single-node** convenience, not the production HA topology.

## Consequences

- Outbound IPs are stable and warmable, protecting deliverability.
- Capacity grows by sizing nodes and adding shards, not by autoscaling
  (mail nodes are explicitly **not** autoscaled).
- Provisioning is Terraform/automation-driven
  (`deploy/terraform/shard/`, `scripts/provision-shard.sh`), and shard
  failover is automatic in the proxy with a manual drain path for
  maintenance (see [upgrade](../operator/upgrade.md#4-roll-the-stalwart-shards-when-needed)).
- Operators run two different substrates (k8s for the control plane,
  VMs for mail) — accepted because the constraints genuinely differ.
