#!/usr/bin/env bash
# scripts/loadtest/chaos-valkey.sh — kill Valkey and verify rate
# limiting fails open (requests pass through) rather than failing
# closed. The KMail rate-limit middleware is configured to log the
# Valkey miss and admit the request; a regression that flips it to
# fail-closed would surface here as a sudden drop in success rate.
set -euo pipefail

PROJECT="${KMAIL_COMPOSE_PROJECT:-kmail}"
# docker-compose.yml pins `container_name: kmail-valkey` (no compose
# `-1` index suffix), so default to that rather than the generated
# `${PROJECT}-valkey-1` name, which does not exist in this stack.
VALKEY_CONTAINER="${KMAIL_VALKEY_CONTAINER:-${PROJECT}-valkey}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8088}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-100}"
MAX_TIME="${KMAIL_CHAOS_MAX_TIME:-10}"
# A request that traverses the Valkey-backed rate limiter. The old
# default (`/api/v1/health`) is not a registered route (404), which
# made the fail-open assertion pass trivially without ever exercising
# the limiter. `/api/v1/admin/feature-flags` is a real authenticated
# endpoint mounted behind the limiter. NOTE: the limiter is gated by
# KMAIL_RATELIMIT_ENABLED (default off in dev) and must be configured
# fail-open (KMAIL_RATELIMIT_FAIL_CLOSED=false) for this test to be
# meaningful — see docs/LOADTEST.md.
ENDPOINT="${KMAIL_CHAOS_VALKEY_ENDPOINT:-/api/v1/admin/feature-flags}"

echo "chaos-valkey: killing $VALKEY_CONTAINER"
docker kill "$VALKEY_CONTAINER" >/dev/null
# Always bring Valkey back, even if an assertion below fails under
# `set -e`; otherwise a failed chaos run leaves the stack degraded.
trap 'docker start '"$VALKEY_CONTAINER"' >/dev/null 2>&1 || true' EXIT

succ=0
for _ in $(seq 1 "$ITERATIONS"); do
  # `|| echo 000` keeps the loop alive under `set -e`: curl exits
  # non-zero on a connect error / --max-time timeout, which would
  # otherwise abort the whole script via the failed assignment.
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time "$MAX_TIME" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        "$JMAP_URL$ENDPOINT" || echo 000)
  # Count only genuine 2xx/3xx as fail-open success. curl emits "000"
  # on a connect error or --max-time timeout; the old `-lt 500` test
  # mis-counted those as success.
  if [ "$code" -ge 200 ] && [ "$code" -lt 400 ]; then
    succ=$((succ+1))
  fi
done

echo "chaos-valkey: restarting $VALKEY_CONTAINER"
docker start "$VALKEY_CONTAINER" >/dev/null
sleep 3

ratio=$(awk -v s="$succ" -v t="$ITERATIONS" 'BEGIN{printf "%.2f", 100.0*s/t}')
echo "chaos-valkey: open=${succ}/${ITERATIONS} (${ratio}%)"
awk -v r="$ratio" 'BEGIN{exit !(r+0 >= 95.0)}'
