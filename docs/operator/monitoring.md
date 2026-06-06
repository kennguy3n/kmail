# Monitoring & alerting guide

KMail exposes Prometheus metrics and ships structured JSON logs. This
guide covers what to scrape, the alert rules that ship with the repo,
the Grafana dashboards, and log shipping. For bringing the local
Grafana/Loki stack up during development, see
[`../DEVELOPMENT.md`](../DEVELOPMENT.md) §9.

## Scrape targets

Two processes expose `/metrics` and must **both** be scraped — they own
different series:

| Job | Default port | Exposes |
| --- | ------------ | ------- |
| `kmail-api` | `8088` (host dev) / `8080` (in-pod) | HTTP request + JMAP proxy metrics (`kmail_http_*`, `kmail_jmap_proxy_*`). |
| `kmail-worker` | `8090` | Supervisor health (`kmail_worker_*`) and the job counters the workers own (`kmail_export_*`, `kmail_search_cutover_*`, retention, deliverability, `kmail_admin_sessions_expired_total`). |

The committed [`deploy/prometheus/prometheus.yml`](../../deploy/prometheus/prometheus.yml)
uses `host.docker.internal` static targets for the local compose stack;
in production replace these with service discovery for the `kmail-api`
and `kmail-worker` pods. The background workers run out-of-process
(`kmail-api` sets `KMAIL_DISABLE_WORKERS=true`), which is why the
`kmail_worker_*` series are **only** on the worker target.

## Key metrics

| Metric | Use |
| ------ | --- |
| `kmail_http_requests_total{status}` | Request rate and error ratio (5xx / total). |
| `kmail_http_request_duration_seconds_bucket` | API latency percentiles. |
| `kmail_jmap_proxy_duration_seconds_bucket` | BFF→Stalwart proxy latency. |
| `kmail_worker_restarts_total` | Worker restart rate (supervisor restarts crashed workers forever). |
| `kmail_worker_panics_total` | Worker panic rate — would have been fatal in the old single-binary model. |

## Alert rules

[`deploy/prometheus/alerts.yml`](../../deploy/prometheus/alerts.yml)
ships ready-to-load rules. Load them and point Prometheus at an
Alertmanager (template:
[`deploy/prometheus/alertmanager.tmpl.yml`](../../deploy/prometheus/alertmanager.tmpl.yml)):

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/alerts.yml
```

Every alert carries a `severity` label the Alertmanager template routes
on: **`page`** (wake someone) vs **`ticket`** (handle in hours).

| Alert | Severity | Fires when |
| ----- | -------- | ---------- |
| `KmailTargetDown` | page | A `kmail-*` scrape target is unreachable >2m. |
| `KmailApiHighErrorRate` | page | >5% of API requests are 5xx over 5m (with meaningful traffic). |
| `KmailApiHighLatencyP99` | ticket | API p99 latency >1s for 10m. |
| `KmailJmapProxyHighLatencyP99` | ticket | JMAP proxy p99 latency >2s for 10m. |
| `KmailWorkerDown` | page | The worker target is unreachable. |
| `KmailWorkerCrashLooping` | — | Elevated `kmail_worker_restarts_total` rate. |
| `KmailWorkerPanicking` | — | Elevated `kmail_worker_panics_total` rate. |
| `KmailRetentionErrors` | — | Retention worker is erroring. |
| `KmailExportJobsFailing` | — | Export fan-out jobs failing. |
| `KmailSearchCutoverFailing` | — | Meilisearch→OpenSearch cutover erroring. |
| `KmailStorageReconcileErrors` | — | Blob-store reconcile errors. |
| `KmailSignupFailureSpike` | — | Self-service signup failure spike. |

The alert expressions reference only metrics the code actually exports
(`internal/middleware/metrics.go`, `cmd/kmail-worker/supervise.go`, and
the per-feature counters) — keep them in sync when metric names change.

## Grafana dashboards

Three dashboards live under
[`deploy/grafana/dashboards/`](../../deploy/grafana/dashboards/) and are
provisioned by `deploy/grafana/provisioning/dashboards.yml`:

- **KMail — Overview** (`kmail-overview.json`): request rate, P50–P99
  latency, active tenants, seats by plan, JMAP proxy latency, retention
  worker counters, rolling 30-day availability SLO, and a Loki panel
  for recent `kmail-api` errors.
- **KMail — Deliverability** (`kmail-deliverability.json`): bounce rate
  (hard/soft), complaint rate per IP pool, suppression list size, DMARC
  pass rate, IP pool reputation, warmup progress, abuse score
  distribution, and a Loki panel for bounce/complaint events.
- **KMail — Tenant Health** (`kmail-tenant-health.json`): per-tenant
  drill-down.

Datasources (Loki + Prometheus) are provisioned by
[`deploy/grafana/datasources.yml`](../../deploy/grafana/datasources.yml).

## Log shipping

KMail's request logger (`internal/middleware/logger.go`) emits
structured JSON when `KMAIL_LOG_FORMAT=json` (the chart default —
`kmailApi.config.KMAIL_LOG_FORMAT: "json"`). Promtail's pipeline
([`deploy/promtail/promtail.yml`](../../deploy/promtail/promtail.yml))
extracts `tenant_id`, `route`, `status_class`, and `method` from each
line and ships to Loki ([`deploy/loki/loki.yml`](../../deploy/loki/loki.yml),
port `3100`). If the BFF falls back to text format, Promtail refuses the
records — keep `KMAIL_LOG_FORMAT=json` in production.
