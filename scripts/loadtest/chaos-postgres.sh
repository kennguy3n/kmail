#!/usr/bin/env bash
# scripts/loadtest/chaos-postgres.sh — pause Postgres for N
# seconds and verify the BFF graceful-degradation middleware
# returns cached responses (200 / 503 with cache headers) rather
# than hard-failing the request.
#
# The script:
#   1. Runs a quick warmup against a read-mostly endpoint so the
#      cache is populated.
#   2. `docker pause`s the Postgres container.
#   3. Issues 50 identical requests and counts how many succeed
#      (status < 500).
#   4. Unpauses Postgres and verifies the warmup endpoint serves
#      cleanly afterwards.
set -euo pipefail

PROJECT="${KMAIL_COMPOSE_PROJECT:-kmail}"
# docker-compose.yml pins `container_name: kmail-postgres` (no compose
# `-1` index suffix), so default to that rather than the generated
# `${PROJECT}-postgres-1` name, which does not exist in this stack.
PG_CONTAINER="${KMAIL_PG_CONTAINER:-${PROJECT}-postgres}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8088}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-50}"
# Per-request ceiling so a Postgres outage cannot hang the harness
# indefinitely (a control-plane read with no cached fallback blocks
# until the upstream returns). curl emits "000" when this trips.
MAX_TIME="${KMAIL_CHAOS_MAX_TIME:-5}"
# Minimum %% of reads that must stay served while Postgres is paused for
# this harness to PASS (exit 0). Defaults to 0 = report-only, because
# control-plane reads currently have NO cached fallback: the
# graceful-degradation middleware (internal/middleware/degradation.go)
# is implemented + unit-tested but not yet wired into cmd/kmail-api, so
# every read hangs to --max-time and returns "000". The harness still
# prints the honest served ratio either way; raise this (e.g. =50) to
# enforce the resilience SLO once the middleware is wired in. See
# docs/BENCHMARKS.md Step 2.
MIN_SUCCESS_PCT="${KMAIL_CHAOS_PG_MIN_SUCCESS_PCT:-0}"
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
  # Only genuine 2xx/3xx counts. A timeout ("000") or 5xx is a
  # failure; the old `-lt 500` test counted "000" timeouts as
  # success, masking the fact that reads hang under DB outage.
  if [ "$code" -ge 200 ] && [ "$code" -lt 400 ]; then
    succ=$((succ+1))
  fi
done
docker unpause "$PG_CONTAINER" >/dev/null
trap - EXIT
sleep 5
curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$JMAP_URL$ENDPOINT" >/dev/null

pct=$(awk -v s="$succ" -v t="$ITERATIONS" 'BEGIN{printf "%.0f", 100.0*s/t}')
echo "chaos-postgres: degraded responses ${succ}/${ITERATIONS} (${pct}% served)"
if [ "$MIN_SUCCESS_PCT" -gt 0 ]; then
  echo "chaos-postgres: enforcing >= ${MIN_SUCCESS_PCT}% served"
  awk -v p="$pct" -v m="$MIN_SUCCESS_PCT" 'BEGIN{exit !(p+0 >= m+0)}'
else
  echo "chaos-postgres: report-only (set KMAIL_CHAOS_PG_MIN_SUCCESS_PCT to enforce an SLO) — control-plane reads have no cached fallback yet; see docs/BENCHMARKS.md Step 2"
fi
