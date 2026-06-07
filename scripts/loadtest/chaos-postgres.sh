#!/usr/bin/env bash
# scripts/loadtest/chaos-postgres.sh — pause Postgres for N seconds
# and measure how the BFF control-plane read path behaves while the
# database is frozen.
#
# Two distinct properties are measured:
#   * bounded  — the request returns a real HTTP status within the
#                per-request ceiling (i.e. the server does NOT hang).
#                This is the property the control-plane read timeout
#                (featureflags.Store, KMAIL_FLAGS_READ_TIMEOUT, default
#                5s) now guarantees: a frozen DB yields a fast,
#                retryable 503 instead of a request that blocks until
#                the client gives up.
#   * served   — the request returned 2xx/3xx. The admin GET now keeps a
#                last-known-good snapshot and serves it (200, tagged
#                `X-Kmail-Stale: true` + `Warning: 110`) when Postgres is
#                unavailable, so a *warmed* endpoint stays ~100% served
#                through a full outage. Only a cold process (no snapshot
#                cached yet) degrades to a retryable 503 — see
#                docs/BENCHMARKS.md Step 2.
#
# The script:
#   1. Runs a quick warmup against a read-mostly endpoint.
#   2. `docker pause`s the Postgres container.
#   3. Issues ITERATIONS identical requests, counting `bounded` and
#      `served`.
#   4. Unpauses Postgres and verifies the endpoint serves cleanly
#      afterwards.
set -euo pipefail

PROJECT="${KMAIL_COMPOSE_PROJECT:-kmail}"
# docker-compose.yml pins `container_name: kmail-postgres` (no compose
# `-1` index suffix), so default to that rather than the generated
# `${PROJECT}-postgres-1` name, which does not exist in this stack.
PG_CONTAINER="${KMAIL_PG_CONTAINER:-${PROJECT}-postgres}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8088}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-50}"
# Per-request ceiling for the probe. Kept comfortably ABOVE the server's
# control-plane read timeout (KMAIL_FLAGS_READ_TIMEOUT, default 5s) plus
# the serve-stale overhead so the probe observes the server's bounded
# response (a fast 503 cold, or a stale 200 warm) rather than tripping
# its own client-side timeout first; curl only emits "000" if the server
# itself fails to bound the read (the regression this harness guards
# against). Measured steady-state is ~7.1s/request (5s read timeout +
# ~2s overhead), so 10s leaves headroom.
MAX_TIME="${KMAIL_CHAOS_MAX_TIME:-10}"
# Minimum %% of reads that must stay *served* (2xx/3xx) while Postgres
# is paused for this harness to PASS (exit 0). Defaults to 100 now that
# the admin GET serves a last-known-good snapshot during an outage: the
# warmup loop below primes that snapshot, so every probe should return a
# stale 200. Set to 0 for report-only (e.g. when probing a build that
# predates the cached-read fallback, or an endpoint that has no
# snapshot). This is SEPARATE from the bounded check below.
MIN_SUCCESS_PCT="${KMAIL_CHAOS_PG_MIN_SUCCESS_PCT:-100}"
# Minimum %% of reads that must be *bounded* (return any HTTP status
# within MAX_TIME rather than hanging to a "000" client timeout) for the
# harness to PASS. Defaults to 100: with the control-plane read timeout
# wired in, a frozen DB must ALWAYS fail fast. Set to 0 to disable (e.g.
# when probing a build that predates the read-timeout fix).
MIN_BOUNDED_PCT="${KMAIL_CHAOS_PG_MIN_BOUNDED_PCT:-100}"
# Real Postgres-backed read endpoint. The old default
# (`/api/v1/feature-flags`) is not a registered route (404), so the
# `< 500` success test passed without touching Postgres at all. The
# admin feature-flags route reads the control-plane DB.
ENDPOINT="${KMAIL_CHAOS_PG_ENDPOINT:-/api/v1/admin/feature-flags}"

echo "chaos-postgres: warming cache for $ENDPOINT"
for _ in $(seq 1 5); do
  curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$JMAP_URL$ENDPOINT" >/dev/null
done

# Postgres stays paused for the duration of the probe loop below
# (the measurement window): up to ITERATIONS x MAX_TIME seconds, each
# request timing out against the frozen DB. There is no separate fixed
# sleep — the loop *is* the outage window — then we unpause.
echo "chaos-postgres: pausing $PG_CONTAINER for the ${ITERATIONS}-probe window (<= ${ITERATIONS}x${MAX_TIME}s)"
docker pause "$PG_CONTAINER" >/dev/null
trap 'docker unpause '"$PG_CONTAINER"' >/dev/null || true' EXIT

succ=0
bounded=0
for _ in $(seq 1 "$ITERATIONS"); do
  # `-w "%{http_code}"` already emits the status (000 when there is no
  # response), so `|| true` is enough to keep the loop alive under
  # `set -e` when curl exits non-zero on a connect error / --max-time
  # timeout. The `${code:-000}` guard normalises the rare case where
  # curl prints nothing at all to a single "000" (not a doubled value).
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time "$MAX_TIME" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        "$JMAP_URL$ENDPOINT" || true)
  code=${code:-000}
  # bounded: any real HTTP status (incl. the expected fast 503) proves
  # the server returned within MAX_TIME instead of hanging. "000" means
  # curl hit its own ceiling first — i.e. the server did NOT bound the
  # read, the regression this harness now catches.
  if [ "$code" != "000" ]; then
    bounded=$((bounded+1))
  fi
  # served: only genuine 2xx/3xx. A 503/5xx or "000" is not served; the
  # old `-lt 500` test counted "000" timeouts as success, masking that
  # reads hung under a DB outage.
  if [ "$code" -ge 200 ] && [ "$code" -lt 400 ]; then
    succ=$((succ+1))
  fi
done
docker unpause "$PG_CONTAINER" >/dev/null
trap - EXIT
sleep 5
curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$JMAP_URL$ENDPOINT" >/dev/null

pct=$(awk -v s="$succ" -v t="$ITERATIONS" 'BEGIN{printf "%.0f", 100.0*s/t}')
bpct=$(awk -v b="$bounded" -v t="$ITERATIONS" 'BEGIN{printf "%.0f", 100.0*b/t}')
echo "chaos-postgres: bounded responses ${bounded}/${ITERATIONS} (${bpct}% returned within ${MAX_TIME}s, no hang)"
echo "chaos-postgres: served responses  ${succ}/${ITERATIONS} (${pct}% 2xx/3xx)"

# Bounded liveness is the core guarantee of the control-plane read
# timeout and is enforced unconditionally (default 100%): a frozen DB
# must fail fast, never hang.
if [ "$MIN_BOUNDED_PCT" -gt 0 ]; then
  echo "chaos-postgres: enforcing >= ${MIN_BOUNDED_PCT}% bounded (fail-fast, no hang)"
  if ! awk -v p="$bpct" -v m="$MIN_BOUNDED_PCT" 'BEGIN{exit !(p+0 >= m+0)}'; then
    echo "chaos-postgres: FAIL — reads hung past ${MAX_TIME}s; is the control-plane read timeout (KMAIL_FLAGS_READ_TIMEOUT) wired in and below MAX_TIME?" >&2
    exit 1
  fi
fi

# Served ratio is now enforced by default: the admin GET serves its
# last-known-good snapshot during an outage, so a warmed endpoint stays
# served. A shortfall means the cached-read fallback regressed (or the
# snapshot was never warmed).
if [ "$MIN_SUCCESS_PCT" -gt 0 ]; then
  echo "chaos-postgres: enforcing >= ${MIN_SUCCESS_PCT}% served (stale snapshot fallback)"
  if ! awk -v p="$pct" -v m="$MIN_SUCCESS_PCT" 'BEGIN{exit !(p+0 >= m+0)}'; then
    echo "chaos-postgres: FAIL — served ${pct}% < ${MIN_SUCCESS_PCT}%; did the stale-snapshot fallback regress, or was the endpoint not warmed first?" >&2
    exit 1
  fi
else
  echo "chaos-postgres: served ratio is report-only (KMAIL_CHAOS_PG_MIN_SUCCESS_PCT=0)"
fi
