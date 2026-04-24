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
  --jmap-url http://localhost:8080 \
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

## Adding new benchmarks

Follow the pattern in `scripts/bench/bench-jmap.go`: warm-up
iterations are always discarded, results are sorted once at the
end, and the P50/P95/P99 computation uses the nearest-rank
method (conservative, matches `python3 statistics`'s percentile
behaviour). Emit JSON on stderr so CI can scrape it.
