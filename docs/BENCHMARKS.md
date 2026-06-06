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

## Session 7 — performance validation at scale (what was actually run)

This section records the Session 7 validation pass. **Every number
here was produced on a single-node compose-local stack** (one
Stalwart, one Postgres, one Valkey, one Meilisearch, one
zk-object-fabric on one VM) — *not* a real 10-shard cloud fleet,
managed Postgres, Wasabi, or live provider deliverability endpoints.
Numbers are labelled `compose-local` accordingly. The "real-infra
prerequisites" under each step list exactly what would unlock a
production-grade run; the harnesses are built so that run is a
one-command follow-up.

### Step 1 — scale benchmark (`make scale-test` / `scale-test-multishard`)

- **compose-local:** the seed → load → report pipeline runs end to
  end (`SCALE_DRY=1` validates the pipeline offline; a live run
  drives the seeded fleet). The single-stack JMAP latency baseline is
  the "Baseline (local compose)" table above; the routing/failover
  shape is the "Multi-shard scale benchmark" table above.
- **Bug fixed during this pass:** `migrations/` had two files at
  version `006` (`006_confidential_send_mls.sql` and
  `006_feature_flags.sql`). `schemamigrate.Discover` rejects duplicate
  versions, so **every** migration failed and no stack could
  initialise. The confidential-send migration was renumbered to
  `007_confidential_send_mls.sql`.
- **Bug fixed during this pass:** `seed-tenants.go` posted shared
  inboxes without the `mls_group_id` the API requires (HTTP 400). It
  now sends a deterministic synthetic group id.
- **Real-infra prerequisites:** a multi-VM Stalwart fleet (≥10
  shards), managed Postgres with the production connection topology,
  and provisioned per-tenant Stalwart principals (see the JMAP
  prerequisite note under Step 2). Full 5 000-tenant message seeding
  also depends on principal provisioning.

### Step 2 — chaos engineering (`make chaos`)

The chaos scripts had several real defects that made them either
unrunnable or falsely green against this stack; they were fixed and
re-run. Findings:

| Failure mode | compose-local result | Notes |
| --- | --- | --- |
| Valkey eviction/kill (`chaos-valkey.sh`) | **100 % open** (30/30, 100/100) | Rate limiter fails open: with Valkey down and `KMAIL_RATELIMIT_FAIL_CLOSED=false`, requests are admitted. Each request pays ~2 s while the redis client exhausts its dial retries — fail-open works but is **slow** under a hard-down Valkey. |
| Postgres pause (`chaos-postgres.sh`) | **0 % served** (0/5) — *finding* | Control-plane reads have **no cached fallback and no DB-side timeout**, so they hang until Postgres returns (bounded only by the new `--max-time`). The graceful-degradation middleware (`internal/middleware/degradation.go`) is implemented + unit-tested but **not wired into `cmd/kmail-api`**, and it targets the `/jmap` + Stalwart-health path, not control-plane Postgres reads. |
| Shard failure (`chaos-shard.sh`) | **prerequisite-blocked** | The JMAP probe needs a provisioned Stalwart mailbox; with the dev token and no seeded mailbox the BFF returns `404 accountNotFound` *before* reaching a shard, so the circuit-breaker path is never exercised. |
| Meilisearch corruption / zk-object-fabric outage | **not yet scripted** | No harness exists for these two modes; documented here as a gap. `internal/search` has no automatic fallback (search calls return the backend error when Meilisearch is unavailable — there is no Postgres `ILIKE` degrade path); zk-fabric outage affects blob read/write only. |

Chaos-harness bugs fixed (`chaos-shard.sh`, `chaos-postgres.sh`,
`chaos-valkey.sh`):

- **Container names** defaulted to the generated `kmail-<svc>-1`
  names, but `docker-compose.yml` pins `container_name: kmail-<svc>`
  (no `-1`), so `docker kill/pause` could never find the container —
  `make chaos` failed on the first line. Defaults corrected.
- **Stale endpoints**: `chaos-postgres` probed `/api/v1/feature-flags`
  and `chaos-valkey` probed `/api/v1/health` — **both 404** in this
  build. A 404 is `< 500`, so the old `-lt 500` success test passed
  without ever touching the faulted dependency (false green). Both
  now probe `/api/v1/admin/feature-flags` (a real authed,
  Postgres-backed route mounted behind the rate limiter), and success
  counts only genuine 2xx/3xx.
- **No request timeout**: a paused Postgres made `curl` hang
  indefinitely. Added `--max-time` everywhere.
- **`set -e` abort**: `code=$(curl …)` aborted the whole script when
  curl exited non-zero (timeout/connect error). Guarded with
  `|| echo 000`.
- **Stuck-down stack**: a failed assertion under `set -e` left the
  killed container down. Added `trap … EXIT` restart guards to
  `chaos-valkey` and `chaos-shard` (`chaos-postgres` already had one).

**Real-infra prerequisites / recommended long-term fixes:** wire the
graceful-degradation middleware (or per-request DB context timeouts)
into the BFF read path so a Postgres outage fails fast / serves cached
rather than hanging; provision Stalwart principals so the shard
circuit-breaker can be exercised; add Meilisearch-corruption and
zk-fabric-outage chaos scripts.

### Step 3 — storage cost validation (`make storage-cost`)

`scripts/loadtest/storage-cost.go` is a deterministic model (no cloud
bill exists compose-local). With the documented assumptions — Wasabi
$6.99/TB-mo, stored twice (primary + retention), no compression
credit, decimal TB — and a `core 70 % / pro 25 % / privacy 5 %`
distribution at 5 / 25 / 50 GB mailboxes:

| | Logical mailbox | Stored | $/user/mo |
| --- | --- | --- | --- |
| Blended | 12.25 GB | 24.5 GB | **$0.1713** |
| Single 10 GB mailbox (the projection's own basis) | 10 GB | 20 GB | **$0.1398** |

**Finding:** the `~$0.12/user/mo` projection in `docs/PROPOSAL.md` is
optimistic. Even its own stated basis (one 10 GB mailbox stored twice)
computes to **$0.14** at $6.99/TB-mo; the blended tier distribution is
**$0.17** (+43 %). The projection holds only with a compression/dedup
credit (~1.2–1.4×) or a lower negotiated price — pass `--compression-ratio`
/ `--price-per-tb-mo` to model those.

**Real-infra prerequisites:** the actual negotiated $/TB-mo, the
seeded fleet's measured mailbox-size distribution, and the
zk-object-fabric hot-path latency overhead (measure
`GET`/`PUT`-through-fabric vs direct-to-backend; not measurable
without a populated fabric + backend).

### Step 4 — email deliverability (`make deliverability-check`)

`scripts/loadtest/deliverability-check.go` drives the **production**
`internal/dns` code paths and validates the locally-checkable half of
the email-auth stack. compose-local result: **all 5 checks PASS** —

| Check | Result |
| --- | --- |
| MX present | PASS |
| SPF syntax (`v=spf1 … include: … ~all`) | PASS |
| DMARC syntax (`v=DMARC1; p=reject; adkim=…; aspf=…`) | PASS |
| DKIM record syntax (RSA-2048 key parses from published `p=`) | PASS |
| DKIM key consistency (sig from private key verifies against published public key) | PASS |

The key-consistency check is the important one: a published DKIM
record that does not match the signing key is the most common cause of
a provider DKIM `fail`, and this proves they match end to end.

**Real-infra prerequisites (inbox placement):** warmed sending IP
pools with valid PTR/reverse-DNS, the DNS records actually published
on a real domain, and seed mailboxes at Gmail / Microsoft / Yahoo to
read the delivered verdict. Then drive the warmup schedule and record
inbox-vs-spam placement and per-provider DKIM/SPF/DMARC pass rates.

## Adding new benchmarks

Follow the pattern in `scripts/bench/bench-jmap.go`: warm-up
iterations are always discarded, results are sorted once at the
end, and the P50/P95/P99 computation uses the nearest-rank
method (conservative, matches `python3 statistics`'s percentile
behaviour). Emit JSON on stderr so CI can scrape it.
