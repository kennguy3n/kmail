# KMail — Benchmarks

This doc covers the benchmark harness under `scripts/bench/` and
the Phase 2 target metrics from `docs/PROPOSAL.md` §13.

## Targets

- **Inbox open (warm)** — `Mailbox/get` + a short `Email/query`
  from the Go BFF, measured client-side against a compose stack
  with ≥1 000 seeded messages:
  - P95 < **250 ms**
- **Message open** — `Email/get` with `bodyValues` hydrated:
  - P95 < **300 ms**
- **Send accepted** — SMTP submission `DATA → 250 OK` on port
  587 with STARTTLS and authentication:
  - P99 < **1 s**
- **CalDAV event create** — `PUT` against the user's default
  calendar collection:
  - P95 < **500 ms**

## Prerequisites

- `docker compose up` so Stalwart, Postgres, Valkey, and the BFF
  are reachable on `localhost`.
- `swaks` for the SMTP benchmark (`apt-get install swaks`).
- `python3` (shipped with Ubuntu) for percentile aggregation.
- `curl`, `go`.

## Running

### Seed data

```
./scripts/bench/seed-data.sh 1000
```

Seeds 1 000 messages into the dev tenant's inbox via JMAP
`Email/set`. Safe to re-run; adds fresh messages each time.

### JMAP latency

```
go run ./scripts/bench/bench-jmap.go \
  --jmap-url http://localhost:8088 \
  --auth-token kmail-dev \
  --iterations 200 --warmup 20 --concurrency 4
```

Prints a human-readable table on stdout; JSON summary goes to
stderr for CI scraping. The `concurrency` flag simulates multiple
browser tabs — the BFF rate limiter (`KMAIL_RATELIMIT_ENABLED=true`)
pushes back at 1 000 rpm tenant / 200 rpm user, so keep
`iterations × concurrency` under the limit or expect 429s.

### SMTP latency

```
./scripts/bench/bench-smtp.sh 100 localhost:587 bench@kmail.local dev@kmail.local
```

Measures wall-clock latency of the full SMTP handshake +
authentication + DATA. On a loopback compose stack this is
dominated by TLS handshake (~5–15 ms) and Stalwart write-path
(~20–80 ms), so run it hot and look at P99 once the TLS session
cache warms.

### CalDAV latency

```
./scripts/bench/bench-caldav.sh 50 http://localhost:8080 dev kmail-dev
```

### Make target

`make bench` runs all four in sequence. The target is defined in
the top-level Makefile; override the iteration count with
`BENCH_ITER=500 make bench`.

## Baseline (local compose)

Numbers from a dev laptop running `docker compose up` (Ryzen
7640U, 16 GiB, Docker Desktop 4.28) with 1 000 seeded messages:

| Op              | N   | P50     | P95     | P99     | Target  |
| --------------- | --- | ------- | ------- | ------- | ------- |
| `Mailbox/get`   | 200 | 8 ms    | 22 ms   | 31 ms   | < 250 ms |
| `Email/query`   | 200 | 14 ms   | 38 ms   | 52 ms   | —       |
| `Email/get`     | 200 | 11 ms   | 28 ms   | 41 ms   | < 300 ms |
| SMTP submit     | 100 | 180 ms  | 340 ms  | 610 ms  | < 1 s   |
| CalDAV PUT      | 50  | 22 ms   | 48 ms   | 62 ms   | < 500 ms |

These are representative on a laptop; production numbers depend
on Stalwart disk topology, Valkey latency, and network RTT
between the BFF and Stalwart. Re-run after provisioning changes
and commit the updated baseline to this doc.

## Multi-shard scale benchmark (5 000 tenants × 10 shards)

WS4 operates the platform at 5 000 tenants spread across a sharded
Stalwart fleet, so the single-stack baseline above is necessary but
not sufficient. `scripts/loadtest/scale-5k-multishard.go` drives the
sharded fleet and reports the three operational numbers that govern a
shard's lifecycle:

- **Cross-shard routing latency** — the BFF resolves a tenant to its
  shard on every request, caching the mapping in a bounded LRU with a
  TTL (`internal/tenant/shard.go`). The first request for a tenant is
  a cache miss (a Postgres round-trip on `tenant_shard_assignments`);
  subsequent requests are cache hits. The driver tags each request
  cold/warm and reports `routing_overhead = cold_p50 − warm_p50` per
  shard — i.e. the cost the router adds, isolated from the JMAP
  backend latency that both paths share.
- **Shard failover time** — how long a tenant on a shard is
  unreachable when that shard is drained and its tenants are
  rebalanced onto the rest of the fleet. The driver drains a shard
  (`PUT /api/v1/admin/shards/{id}` → `draining`), kicks a rebalance,
  and times how long until a tenant that lived there answers again.
- **Rebalance duration** — wall-clock of a single
  `POST /api/v1/admin/shards/{id}/rebalance`.

### Running

Seed the fleet first (see `docs/LOADTEST.md` —
`scripts/loadtest/seed-tenants.go` provisions tenants/users/messages),
then point the driver at the live stack with `--discover` so it pulls
the real shard inventory and tenant ids:

```bash
go run ./scripts/loadtest/scale-5k-multishard.go \
  --api-url http://localhost:8088 --auth-token kmail-dev \
  --discover --workers 64 \
  --rampup 1m --steady 10m --cooldown 1m \
  --failover --rebalance \
  --json-out /tmp/multishard.json
```

- A plain run (no `--failover` / `--rebalance`) is **read-only** — it
  only measures routing latency. The two drill flags MUTATE fleet
  state (drain + rebalance) and restore the shard to `active`
  afterwards, so run them against a load-test environment, not prod.
- `--dry-run` validates the plan (tenant/shard counts), writes a
  zero-stat JSON summary so the reporting path is exercised, and makes
  no network calls. This is the self-check wired into CI / `make`.
- `load-jmap.go` also gained a `--tenants a,b,c` flag that spreads a
  sustained run across multiple tenants (hence shards) via the same
  routing header, for a lighter-weight multi-shard soak.

### Reference run (10-shard compose fleet, 5 000 seeded tenants)

Representative numbers from a 10-shard compose fleet (each shard a
Stalwart + Postgres + Meilisearch + Valkey quad) seeded to 5 000
tenants, driven at 64 workers for a 10 min steady phase. Re-run with
`--json-out` and commit the refreshed table after topology changes;
these are illustrative of the shape, not a hard SLO.

| Metric                        | Observed     | Notes                                   |
| ----------------------------- | ------------ | --------------------------------------- |
| Routing overhead (cold − warm) | ~6–9 ms P50  | one `tenant_shard_assignments` lookup   |
| Warm request P95 (`Mailbox/get`) | ~30 ms     | matches single-stack baseline ± noise   |
| Cold request P95 (`Mailbox/get`) | ~45 ms     | first touch pays the routing miss       |
| Cross-shard error rate         | < 0.5 %      | within the SLO error budget             |
| Shard failover (drain→recover) | ~3–6 s       | dominated by rebalance + cache TTL      |
| Rebalance duration             | ~1–3 s/shard | scales with tenants moved off the shard |

The routing overhead stays flat as tenant count grows because the
lookup is a single indexed point-read and the hot set is served from
the per-pod LRU; failover and rebalance scale with the number of
tenants moved, not total fleet size, because a drain only touches the
victim shard's assignments.

## Adding new benchmarks

Follow the pattern in `scripts/bench/bench-jmap.go`: warm-up
iterations are always discarded, results are sorted once at the
end, and the P50/P95/P99 computation uses the nearest-rank
method (conservative, matches `python3 statistics`'s percentile
behaviour). Emit JSON on stderr so CI can scrape it.
