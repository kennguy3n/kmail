# KMail monitoring & alerting

This directory holds the observability stack config: Grafana dashboards
+ provisioning (`grafana/`), Prometheus scrape config + alert rules
(`prometheus/`), and the Loki/Promtail log pipeline. The Helm chart can
ship the same dashboards and alert rules into a Prometheus Operator
cluster (`helm/kmail` — `serviceMonitor`, `prometheusRule`,
`grafanaDashboards`).

## Dashboards (`grafana/dashboards/`)

Provisioned automatically (`grafana/provisioning/dashboards.yml`) and,
in-cluster, mirrored into the chart by
`scripts/helm-sync-dashboards.sh` and mounted via the
`grafanaDashboards` ConfigMap.

| Dashboard (uid) | What it shows | Data source |
| --- | --- | --- |
| `kmail-overview` | BFF request rate, latency, tenants/seats, SLO gauge. | app metrics |
| `kmail-deliverability` | Bounce/complaint/DMARC/IP-pool reputation. | app metrics + Loki |
| `kmail-tenant-health` | Per-tenant drill-down (request rate, errors, latency). | Loki |
| `kmail-onboarding-funnel` | Signup funnel: initiated → completed conversion, failures, rate-limits, replays, by plan. | `kmail_signup_*` |
| `kmail-shard-health` | Per-shard (Stalwart StatefulSet) pod readiness, PVC utilisation, restarts, CPU/mem, JMAP latency. | kube-prometheus + app metrics |
| `kmail-cost-per-tenant` | Estimated infra $/month and $/tenant from measured CPU/mem/PVC × tunable unit-price variables. | kube-prometheus + app metrics |
| `kmail-error-budget` | Availability SLO, budget remaining, multi-window burn rates (matches the burn-rate alerts). | `kmail_http_requests_total` |

> **kube-prometheus dependency:** `kmail-shard-health` and
> `kmail-cost-per-tenant` read `kube_*` (kube-state-metrics) and
> `container_*` / `kubelet_volume_stats_*` (cAdvisor/kubelet) series.
> Those are standard in a `kube-prometheus-stack` install — the same
> stack the chart's `ServiceMonitor` / `PrometheusRule` target. The
> other dashboards use only kmail's own `kmail_*` metrics + Loki.
> `kmail-cost-per-tenant` unit prices ($/core-hour, $/GiB-hour,
> $/GiB-month) are dashboard variables — set them to your cloud's
> rates; the numbers are a planning estimate, not billing truth.

## Alert rules (`prometheus/alerts.yml`)

Loaded by the compose/standalone Prometheus. The chart ships the same
family **plus** the SLO recording rules as a `PrometheusRule` CRD
(`helm/kmail/templates/prometheusrule.yaml`); the two are kept in sync.

Every alert carries a `severity` label the Alertmanager template routes
on:

- `severity: page` → **PagerDuty** (wakes someone): target-down, API
  5xx spike, worker down/crash-loop, signup broken, and the **fast/
  medium error-budget burn** alerts.
- `severity: ticket` → **Opsgenie** (non-paging queue): latency,
  per-feature job failures, and the **slow/very-slow burn** alerts.

### Error-budget burn rate (multi-window, multi-burn-rate)

Following the Google SRE workbook, against a 99.9% availability SLO
(0.1% budget):

| Alert | Windows | Burn | Budget exhausted in | Routes to |
| --- | --- | --- | --- | --- |
| `KmailErrorBudgetBurnFast` | 5m & 1h | 14.4x | ~2 days | PagerDuty |
| `KmailErrorBudgetBurnMedium` | 30m & 6h | 6x | ~5 days | PagerDuty |
| `KmailErrorBudgetBurnSlow` | 2h & 1d | 3x | ~10 days | Opsgenie |
| `KmailErrorBudgetBurnVerySlow` | 6h & 3d | 1x | ~30 days | Opsgenie |

The short+long window pairing only fires when *both* windows are
burning, which suppresses flapping on brief blips.

## PagerDuty / Opsgenie routing (`prometheus/alertmanager.tmpl.yml`)

A **template**, not a ready-to-run config: integration keys are
`${PAGERDUTY_ROUTING_KEY}` / `${OPSGENIE_API_KEY}` env placeholders to
be rendered from a Secret (e.g. `envsubst` at deploy time, or the
`alertmanager-config` service in `docker-compose.prod.yml`). Never
commit real keys. Route summary:

```
route (default) -> opsgenie-tickets
  ├─ severity="page"   -> pagerduty-pages   (group_wait 10s, repeat 1h)
  └─ severity="ticket" -> opsgenie-tickets  (repeat 4h)
inhibit: KmailTargetDown (page) silences same-job severity=ticket noise
```

To exercise routing locally, supply the two env vars, render the
template, and point Prometheus at an Alertmanager (the `alerting:`
block in `prometheus/prometheus.yml` is commented out by default).
