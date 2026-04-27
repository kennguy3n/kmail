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
  --jmap-url http://localhost:8080 \
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

## Wiring into CI

The chaos suite is intentionally **not** wired to CI today — the
failure modes need a real compose stack and the run takes 5+
minutes. For Phase 7 the harness is run on demand by the SRE team
on the staging cluster. A future phase can promote it to a
nightly job once the chaos toolkit is integrated with the CI
runner pool.
