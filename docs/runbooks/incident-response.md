# Runbook: Incident response

**Goal:** a repeatable triage → mitigate → resolve → learn loop for any
production incident.

## 0. Severity

| Sev | Definition | Examples |
| --- | --- | --- |
| SEV1 | Full or widespread outage; data at risk | Control-plane DB down; all shards unreachable; auth down |
| SEV2 | Major degradation; subset of tenants | One shard down; error-budget burning fast; delivery backlog |
| SEV3 | Minor / single feature | One worker job failing; elevated latency within SLO |

Declare SEV1/SEV2 in the incident channel, assign an **Incident
Commander (IC)**, and start a timeline doc.

## 1. Triage (first 5 minutes)

```bash
# What's firing?  (Alertmanager / Prometheus)
#   Alert rules: deploy/prometheus/alerts.yml + the chart PrometheusRule.
# Top-level health:
kubectl -n kmail get pods
curl -s -H "Authorization: Bearer $KMAIL_ADMIN_TOKEN" "$API/api/v1/admin/shards/health" | jq .
curl -s "$API/healthz"; curl -s "$API/readyz"
```

Map the firing alert to a runbook:

| Alert | Likely cause | Go to |
| --- | --- | --- |
| `KmailTargetDown` | api/worker/stalwart scrape target down | this doc §2; `shard-failover.md` if stalwart |
| `KmailApiHighErrorRate` / `KmailErrorBudgetBurnFast` | BFF 5xx spike | §2 |
| `KmailApiHighLatencyP99` / `KmailJmapProxyHighLatencyP99` | saturation or slow shard | §2, capacity |
| `KmailWorkerDown` / `KmailWorkerCrashLooping` / `KmailWorkerPanicking` | worker crash | §3 |
| `KmailRetentionErrors` / `KmailExportJobsFailing` / `KmailSearchCutoverFailing` / `KmailStorageReconcileErrors` | dependency (S3/search) errors | §3 |
| `KmailSignupFailureSpike` | signup path / OIDC / DB | §2 |

## 2. Mitigate — API / latency / errors

- **Recent deploy?** Roll back first, ask questions later:
  ```bash
  kubectl -n kmail rollout undo deploy/kmail-api
  kubectl -n kmail rollout status deploy/kmail-api --timeout=180s
  ```
- **Saturation?** Check HPA; raise the ceiling if pegged:
  ```bash
  kubectl -n kmail get hpa
  kubectl -n kmail describe hpa kmail-api | tail -20
  helm upgrade kmail ./deploy/helm/kmail --reuse-values --set kmailApi.hpa.maxReplicas=20
  ```
- **One slow shard?** Identify via the per-shard health dashboard →
  `shard-failover.md`.
- **Dependency down** (Postgres/Valkey)? Rate-limiting fails CLOSED in
  prod, so a Valkey outage surfaces as 429/5xx — restore Valkey or, as a
  deliberate emergency lever, relax `KMAIL_RATELIMIT_FAIL_CLOSED` only
  with IC sign-off.

## 3. Mitigate — workers / background jobs

```bash
kubectl -n kmail logs deploy/kmail-worker --tail=200
kubectl -n kmail rollout restart deploy/kmail-worker   # clears a wedged worker
```

Worker jobs are idempotent and resume on the next tick, so a restart is
safe. Persistent failures point at a dependency (S3 blob store, search
backend) — check `kmail_export_*`, `kmail_retention_*`,
`kmail_search_cutover_*`, `kmail_storage_*` metrics and the relevant
upstream.

## 4. Communicate

- Post status every 30 min for SEV1/2 (or on material change).
- Update the public status page if tenant-facing.
- Keep the timeline doc current: detection time, actions, their effect.

## 5. Resolve

- Confirm alerts cleared and the SLO burn rate is back under 1x.
- Confirm the error budget — if a fast-burn alert fired, freeze
  non-critical deploys until budget recovers.
- Downgrade severity, then close.

## 6. Postmortem (within 48h, blameless)

- Timeline, root cause, contributing factors.
- What detected it / what should have.
- Action items with owners (and a check: would the relevant runbook have
  made this faster? update it).
- Link the incident to any new/changed alert thresholds.
