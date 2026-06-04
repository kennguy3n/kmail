# Load testing & chaos engineering

This doc covers the Phase 7 load-testing and chaos-engineering
harness under `scripts/loadtest/`. Two kinds of runs are
supported:

- **Sustained load** — `load-jmap.go` and `load-smtp.sh` push a
  configurable workload through the BFF to characterise the SLO
  envelope. Use these when validating a release candidate or
  baselining a new shard topology.
- **Chaos** — `chaos-shard.sh`, `chaos-postgres.sh`, and
  `chaos-valkey.sh` each kill or pause one dependency and verify
  the BFF degrades gracefully. Use these to catch regressions in
  the circuit breaker, cache fall-through, and fail-open rate
  limiter.

The Makefile exposes both as one-liners:

```bash
make loadtest LOADTEST_ITER=2000 LOADTEST_TPS=50
make chaos
```

## Sustained load — `load-jmap.go`

`load-jmap.go` is a self-contained Go program (build-tag-gated as
`//go:build ignore` so it never lands in the production binary).
The default workload mix is 70 % mailbox list, 15 % email query,
10 % email get, 5 % email send. The mix matches the production
read/write ratio published in `docs/SLO_TRACKER.md`.

```bash
go run ./scripts/loadtest/load-jmap.go \
  --jmap-url http://localhost:8088 \
  --auth-token kmail-dev \
  --concurrency 16 \
  --rampup 30s --steady 120s --cooldown 30s \
  --iterations 1000 \
  --json-out /tmp/loadtest.json
```

Output is the canonical KMail benchmark table:

```
op            n     p50 ms  p95 ms  p99 ms  max ms  err%
------------------------------------------------------------
mailbox_list  701    12.3    27.1    44.2    98.6    0.0
email_query   149    18.0    36.4    52.2   118.4    0.0
email_get     101    21.7    41.0    63.1   142.0    0.0
email_send     49    74.5   118.0   140.0   202.5    0.0
```

### Expected baselines (single-node compose)

| Metric                   | Target  |
|--------------------------|---------|
| `mailbox_list` p95       | ≤ 50 ms |
| `email_query` p95        | ≤ 60 ms |
| `email_get` p95          | ≤ 75 ms |
| `email_send` p95         | ≤ 200 ms |
| Overall error rate       | ≤ 0.1 % |

If a run drops below baseline, capture the BFF + Stalwart logs
along with the JSON output and attach to the regression issue.

## Sustained SMTP load — `load-smtp.sh`

`load-smtp.sh` is a shell loop around `swaks` that submits
messages at a target TPS. It exists because SMTP submission
exercises a different code path than the JMAP `Email/set` send
flow (queue ingestion, SPF / DKIM signing, virus scan).

```bash
scripts/loadtest/load-smtp.sh 25 60   # 25 TPS for 60 seconds
```

## Chaos — what we test, what to look for

| Script                    | Failure injected     | Expected behaviour |
|---------------------------|----------------------|--------------------|
| `chaos-shard.sh`          | Stalwart shard kill  | Circuit breaker opens, secondary shard takes over within the 99.95 % SLO window. |
| `chaos-postgres.sh`       | Postgres pause       | Graceful-degradation middleware serves cached responses; success rate stays ≥ 50 %. |
| `chaos-valkey.sh`         | Valkey kill          | Rate-limit middleware fails open; success rate stays ≥ 95 %. |

Each script sets a non-zero exit code if the SLO target is
missed. Run them inside the compose stack:

```bash
docker compose up -d
make chaos
```

### Interpreting failures

- **Shard chaos failure** — start with the BFF logs around the
  fault window. If you see repeated `circuit-open` followed by
  successful retries, the breaker is working but the SLO budget
  is too tight; check whether the iteration count
  (`KMAIL_CHAOS_ITERATIONS`) exposes more than your shard
  topology can absorb. Genuine regressions look like 503s lasting
  past the breaker recovery window.
- **Postgres chaos failure** — confirm
  `internal/middleware/degraded.go` still serves cached responses
  on the affected route. Cache misses count as failures.
- **Valkey chaos failure** — the rate limiter is the suspect.
  Confirm the middleware logs the Valkey error and admits the
  request.

## Scale load test — 5K-tenant harness

The `load-jmap.go` harness above characterises a single account's
SLO envelope. The **scale harness** answers a different question:
*does the platform hold its SLOs with thousands of tenants worth
of data and a realistic, mixed workload?* It is made of three
self-contained Go programs (all `//go:build ignore`, run with
`go run`) plus a chaos orchestrator, wired together by the
`scale-test` Make target.

```
seed-tenants.go  ──▶  scale-5k.go  ──▶  report.go
 (provision)          (drive load)      (render + verdict)
                          ▲
            chaos-during-load.sh injects faults here
```

### 1. Seed the fleet — `seed-tenants.go`

Provisions a synthetic tenant fleet through the public admin API
and JMAP proxy. Per tenant (all counts configurable): 1 tenant,
N domains, N users, N shared inboxes, N retention policies, N
webhooks, and N inbox messages seeded into the first user.

```bash
go run ./scripts/loadtest/seed-tenants.go \
  --api-url http://localhost:8088 --auth-token kmail-dev \
  --tenants 100 --users 20 --domains 3 \
  --messages 10000 --shared-inboxes 2 \
  --retention 1 --webhooks 1 \
  --concurrency 16
```

It is **idempotent** — every entity is reconciled against current
server state (list-then-create keyed on a natural key: slug,
domain, `kchat_user_id`, address, URL; messages fill the gap to
the target count via an `Email/query calculateTotal` delta) — and
**parallel**: tenants are reconciled by a bounded goroutine pool
(`--concurrency`) and messages by a per-tenant pool
(`--msg-concurrency`) in batched `Email/set` calls
(`--msg-batch`). `--dry-run` prints the provisioning plan and
makes no network calls. Supports up to 5 000 tenants
(`--tenants`).

### 2. Drive the load — `scale-5k.go`

Runs a three-phase test — **ramp-up → steady state → cool-down**
— against the seeded tenants with a weighted workload mix:

| Operation           | Weight | Call |
|---------------------|-------:|------|
| `inbox_open`        |   40 % | JMAP `Mailbox/get` |
| `message_read`      |   20 % | JMAP `Email/query` + `Email/get` (back-ref) |
| `search`            |   15 % | JMAP `Email/query` (text filter) |
| `send`              |   10 % | JMAP `Email/set` (draft create) |
| `calendar`          |    5 % | `GET /api/v1/calendars/{accountId}` |
| `admin_api`         |    5 % | `GET /api/v1/tenants/{id}/users` |
| `attachment_upload` |    5 % | `POST /api/v1/attachments/upload` (multipart) |

```bash
go run ./scripts/loadtest/scale-5k.go \
  --api-url http://localhost:8088 --auth-token kmail-dev \
  --tenants 100 --workers 64 \
  --rampup 1m --steady 10m --cooldown 1m \
  --json-out scale-report.json
```

Concurrency follows the phase shape: the number of active virtual
users ramps linearly 0→`--workers`, holds flat through steady
state, then ramps back down. Per-operation latency percentiles
(P50/P95/P99) are computed from a bounded reservoir
(`--max-samples`, default 200 000) so a multi-million-request run
stays within a fixed memory budget; request/error counts and
throughput buckets (`--bucket`, default 10 s) are exact. The run
writes a JSON summary (`--json-out`) and prints a console table.
`--dry-run` validates the config (including that the weights sum
to 100), prints the plan, and writes a zero-stat summary so the
reporting path can be exercised offline.

### 3. Render the report — `report.go`

Turns the JSON summary into Markdown: run metadata, a
per-operation latency table, throughput over time, an SLO
compliance table, and a **PASS / FAIL** verdict.

```bash
go run ./scripts/loadtest/report.go \
  --in scale-report.json --out scale-report.md
```

The default SLOs are P95 latency ceilings per operation plus a
global error budget; override them with `--slo-file` (JSON):

```json
{"error_budget_pct": 0.5,
 "ops": {"inbox_open": 150, "search": 300}}
```

| Operation           | Default P95 target |
|---------------------|-------------------:|
| `inbox_open`        |  150 ms |
| `message_read`      |  200 ms |
| `search`            |  300 ms |
| `send`              |  400 ms |
| `calendar`          |  250 ms |
| `admin_api`         |  250 ms |
| `attachment_upload` | 2000 ms |
| overall error rate  |  ≤ 0.5 % |

`report.go` exits non-zero when the verdict is FAIL (disable with
`--fail-on-violation=false`) so it can gate a CI job.

### 4. Chaos under load — `chaos-during-load.sh`

Runs the full weighted workload (`scale-5k.go`) against the
seeded fleet, waits for steady state, then fires the existing
`chaos-*.sh` injectors one at a time. Afterwards it renders the
report and prints an impact summary comparing the chaos window
against the steady-state baseline (using the throughput buckets
recorded by `scale-5k.go`; the comparison needs `jq`).

```bash
scripts/loadtest/chaos-during-load.sh                 # full run
scripts/loadtest/chaos-during-load.sh --dry-run       # plan only
```

Override `CHAOS_LOAD_TENANTS`, `CHAOS_LOAD_WORKERS`,
`CHAOS_RAMPUP` / `CHAOS_STEADY` / `CHAOS_COOLDOWN`, and
`CHAOS_SCRIPTS` to shape the run.

### One-liner — `make scale-test`

The Make target stitches seed → load → report together:

```bash
make scale-test TENANTS=100 USERS=10 DURATION=10m
make scale-test SCALE_DRY=1                # offline dry run (CI self-check)
```

`DURATION` is the steady-state duration; `SCALE_RAMPUP`,
`SCALE_COOLDOWN`, `SCALE_WORKERS`, `SCALE_MESSAGES`, and
`SCALE_OUT` (output directory, default `./loadtest-out/`) tune the
rest. `SCALE_DRY=1` runs the whole pipeline without touching the
BFF, which is how the build self-check verifies the harness.

### JSON summary schema

`scale-5k.go` and `report.go` share this shape:

```jsonc
{
  "meta":   { "started_at", "finished_at", "api_url", "tenants",
              "targets", "workers", "rampup_s", "steady_s",
              "cooldown_s", "bucket_s", "attachment_bytes", "dry_run" },
  "operations": [ { "op", "weight", "n", "errors", "error_rate_pct",
                    "p50_ms", "p95_ms", "p99_ms", "max_ms", "mean_ms" } ],
  "buckets":    [ { "index", "start_s", "phase", "requests",
                    "errors", "rps" } ],
  "totals":     { "n", "errors", "error_rate_pct", "rps" }
}
```

## Wiring into CI

The chaos suite runs as a nightly cron in
[`.github/workflows/chaos.yml`](../.github/workflows/chaos.yml)
(item #11 in `docs/PHASES.md`). The workflow spins up the
compose stack, exercises each fault injector (Stalwart restart,
Postgres pause/unpause, Valkey kill, Stalwart kill/restart, and
the deliverability-quarantine + push-fanout chaos jobs), and
fails if a recovery SLO is breached. On-demand runs are still
supported via the `workflow_dispatch` trigger so the SRE team
can replay a specific scenario without waiting for the cron
slot.

The suite is deliberately **not** wired to the PR-blocking
`ci.yml` because individual chaos scenarios take 5+ minutes and
need a full compose stack that pull-request runners cannot
reliably provision under contention. Phase 7 keeps PR latency
short by keeping chaos on the nightly track; daytime regressions
surface through the SLO/runbook dashboards.
