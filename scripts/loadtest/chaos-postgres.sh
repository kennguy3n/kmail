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
PG_CONTAINER="${KMAIL_PG_CONTAINER:-${PROJECT}-postgres-1}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8080}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
PAUSE_S="${KMAIL_CHAOS_PAUSE_S:-15}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-50}"
ENDPOINT="${KMAIL_CHAOS_PG_ENDPOINT:-/api/v1/feature-flags}"

echo "chaos-postgres: warming cache for $ENDPOINT"
for _ in $(seq 1 5); do
  curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$JMAP_URL$ENDPOINT" >/dev/null
done

echo "chaos-postgres: pausing $PG_CONTAINER for ${PAUSE_S}s"
docker pause "$PG_CONTAINER" >/dev/null
trap 'docker unpause '"$PG_CONTAINER"' >/dev/null || true' EXIT

succ=0
for _ in $(seq 1 "$ITERATIONS"); do
  code=$(curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        "$JMAP_URL$ENDPOINT")
  if [ "$code" -lt 500 ]; then
    succ=$((succ+1))
  fi
done
sleep "$PAUSE_S"
docker unpause "$PG_CONTAINER" >/dev/null
trap - EXIT
sleep 5
curl -fsS -H "Authorization: Bearer $AUTH_TOKEN" "$JMAP_URL$ENDPOINT" >/dev/null

echo "chaos-postgres: degraded responses ${succ}/${ITERATIONS}"
test "$succ" -gt $((ITERATIONS / 2))
