# 0004 — Splitting background workers into a separate process

- **Status**: Accepted
- **Related**: [`../../cmd/kmail-worker/`](../../cmd/kmail-worker/), [`../../deploy/helm/kmail/values.yaml`](../../deploy/helm/kmail/values.yaml) (`kmailWorker`), [`../../README.md`](../../README.md)

## Context

Originally `kmail-api` ran the background workers in-process: calendar
reminders, undo/scheduled/snooze dispatch, billing quota scans, search
cutover, deliverability alerts, shard-health probing, retention, export
fan-out, admin-proxy expiry, and webhook delivery. Coupling these to
the request-serving binary meant background load competed with request
latency, a worker panic could threaten request serving, and the two
workloads could not be scaled or resourced independently.

## Decision

Decompose the background workers into a dedicated **`kmail-worker`**
process (the Session 6 decomposition). The Helm chart deploys it as a
separate `Deployment` that reuses the API's ConfigMap and Secret so both
read identical config. It exposes only `/healthz`, `/readyz`, and
`/metrics` (port 8090) — no tenant traffic, no Service/Ingress/HPA.
`kmail-api` sets `KMAIL_DISABLE_WORKERS=true` in this topology.

To make this safe, **every worker claims its unit of work via a
Postgres advisory/row lock (`FOR UPDATE SKIP LOCKED`) or a Valkey
lease**. That invariant lets the API and worker run concurrently during
rollouts and lets the worker scale past one replica without
double-executing.

## Consequences

- Request latency is insulated from background load; each workload
  scales and is resourced independently.
- The worker supervisor restarts a crashed/panicking worker forever
  with capped backoff (no circuit breaker by design), so a broken
  worker never crashes the pod — instead it shows up as
  `kmail_worker_restarts_total` / `kmail_worker_panics_total`, which
  must be alerted on (see [monitoring](../operator/monitoring.md)).
- **Invariant**: any worker registered in `cmd/kmail-worker/workers.go`
  that can also run in-process MUST be concurrency-safe via a lock/lease.
  Registering an unlocked worker would double-execute.
- **Upgrade impact**: non-Helm deployments must deploy `kmail-worker`
  alongside `kmail-api` or set `KMAIL_DISABLE_WORKERS=false` (see the
  [upgrade guide](../operator/upgrade.md#worker-process-decomposition)).
