#!/usr/bin/env bash
# scripts/loadtest/chaos-shard.sh — kill a Stalwart shard
# container and verify the BFF circuit breaker + secondary-shard
# fail-over keeps the JMAP surface inside the 99.95% SLO budget.
#
# The script:
#   1. Snapshots the BFF readyz status pre-fault.
#   2. `docker kill`s the primary Stalwart shard container.
#   3. Drives 200 JMAP calls through the BFF and counts non-2xx.
#   4. Restarts the shard container and confirms readyz returns
#      to OK.
#
# Pass criterion (default): error rate <= 0.05% (== 1 in 200).
set -euo pipefail

PROJECT="${KMAIL_COMPOSE_PROJECT:-kmail}"
SHARD_CONTAINER="${KMAIL_SHARD_CONTAINER:-${PROJECT}-stalwart-1}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8080}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-200}"
SLO_PCT="${KMAIL_CHAOS_SLO_PCT:-0.05}"

echo "chaos-shard: pre-fault /readyz"
curl -fsS "$JMAP_URL/readyz" >/dev/null

echo "chaos-shard: killing $SHARD_CONTAINER"
docker kill "$SHARD_CONTAINER" >/dev/null

errs=0
for i in $(seq 1 "$ITERATIONS"); do
  if ! curl -fsS -X POST "$JMAP_URL/jmap" \
       -H "Authorization: Bearer $AUTH_TOKEN" \
       -H "Content-Type: application/json" \
       -d '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}' \
       >/dev/null 2>&1; then
    errs=$((errs+1))
  fi
done

echo "chaos-shard: restarting $SHARD_CONTAINER"
docker start "$SHARD_CONTAINER" >/dev/null
sleep 5
curl -fsS "$JMAP_URL/readyz" >/dev/null

ratio=$(awk -v errs="$errs" -v iter="$ITERATIONS" 'BEGIN{printf "%.4f", 100.0*errs/iter}')
echo "chaos-shard: errors=${errs}/${ITERATIONS} (${ratio}%) target<=${SLO_PCT}%"
awk -v ratio="$ratio" -v slo="$SLO_PCT" 'BEGIN{exit !(ratio+0 <= slo+0)}'
