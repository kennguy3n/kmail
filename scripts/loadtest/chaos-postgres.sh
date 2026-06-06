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
PAUSE_S="${KMAIL_CHAOS_PAUSE_S:-15}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-50}"
# Per-request ceiling so a Postgres outage cannot hang the harness
# indefinitely (a control-plane read with no cached fallback blocks
# until the upstream returns). curl emits "000" when this trips.
MAX_TIME="${KMAIL_CHAOS_MAX_TIME:-5}"
# Real Postgres-backed read endpoint. The old default
# (`/api/v1/feature-flags`) is not a registered route (404), so the
# `< 500` success test passed without touching Postgres at all. The
# admin feature-flags route reads the control-plane DB.
ENDPOINT="${KMAIL_CHAOS_PG_ENDPOINT:-/api/v1/admin/feature-flags}"

echo "chaos-postgres: warming cache for $ENDPOINT"
for _ in $(seq 1 5); do
  curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$JMAP_URL$ENDPOINT" >/dev/null
done

echo "chaos-postgres: pausing $PG_CONTAINER for ${PAUSE_S}s"
docker pause "$PG_CONTAINER" >/dev/null
trap 'docker unpause '"$PG_CONTAINER"' >/dev/null || true' EXIT

succ=0
for _ in $(seq 1 "$ITERATIONS"); do
  # `|| echo 000` keeps the loop alive under `set -e`: curl exits
  # non-zero on a connect error / --max-time timeout, which would
  # otherwise abort the whole script via the failed assignment.
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time "$MAX_TIME" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        "$JMAP_URL$ENDPOINT" || echo 000)
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

echo "chaos-postgres: degraded responses ${succ}/${ITERATIONS}"
test "$succ" -gt $((ITERATIONS / 2))
