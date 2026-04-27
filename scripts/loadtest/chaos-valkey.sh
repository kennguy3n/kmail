#!/usr/bin/env bash
# scripts/loadtest/chaos-valkey.sh — kill Valkey and verify rate
# limiting fails open (requests pass through) rather than failing
# closed. The KMail rate-limit middleware is configured to log the
# Valkey miss and admit the request; a regression that flips it to
# fail-closed would surface here as a sudden drop in success rate.
set -euo pipefail

PROJECT="${KMAIL_COMPOSE_PROJECT:-kmail}"
VALKEY_CONTAINER="${KMAIL_VALKEY_CONTAINER:-${PROJECT}-valkey-1}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8080}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-100}"
ENDPOINT="${KMAIL_CHAOS_VALKEY_ENDPOINT:-/api/v1/health}"

echo "chaos-valkey: killing $VALKEY_CONTAINER"
docker kill "$VALKEY_CONTAINER" >/dev/null

succ=0
for _ in $(seq 1 "$ITERATIONS"); do
  code=$(curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        "$JMAP_URL$ENDPOINT")
  if [ "$code" -lt 500 ]; then
    succ=$((succ+1))
  fi
done

echo "chaos-valkey: restarting $VALKEY_CONTAINER"
docker start "$VALKEY_CONTAINER" >/dev/null
sleep 3

ratio=$(awk -v s="$succ" -v t="$ITERATIONS" 'BEGIN{printf "%.2f", 100.0*s/t}')
echo "chaos-valkey: open=${succ}/${ITERATIONS} (${ratio}%)"
awk -v r="$ratio" 'BEGIN{exit !(r+0 >= 95.0)}'
