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
# Pass criterion (when enforced): error rate <= 0.05% — the 99.95% SLO
# budget. With ITERATIONS=200 that means 0 failures (1 in 200 is
# already 0.5%, ten times over budget); raise ITERATIONS to absorb
# rare flakes, or override KMAIL_CHAOS_SLO_PCT for looser targets.
#
# Enforcement is PREREQUISITE-AWARE (see KMAIL_CHAOS_SHARD_ENFORCE
# below): the /jmap probe needs a provisioned mailbox, and without one
# every iteration is a 404 regardless of shard health, which would make
# this a guaranteed (false) red against a vanilla compose stack. The
# harness probes once pre-fault and only enforces the SLO when the
# prerequisite is actually met; otherwise it measures and reports but
# exits 0.
set -euo pipefail

PROJECT="${KMAIL_COMPOSE_PROJECT:-kmail}"
# docker-compose.yml pins `container_name: kmail-stalwart` (no compose
# `-1` index suffix), so default to that rather than the generated
# `${PROJECT}-stalwart-1` name, which does not exist in this stack.
SHARD_CONTAINER="${KMAIL_SHARD_CONTAINER:-${PROJECT}-stalwart}"
JMAP_URL="${KMAIL_JMAP_URL:-http://localhost:8088}"
AUTH_TOKEN="${KMAIL_AUTH_TOKEN:-kmail-dev}"
ITERATIONS="${KMAIL_CHAOS_ITERATIONS:-200}"
MAX_TIME="${KMAIL_CHAOS_MAX_TIME:-10}"
SLO_PCT="${KMAIL_CHAOS_SLO_PCT:-0.05}"
# Whether to enforce the fail-over SLO (exit non-zero on a breach).
#   auto (default) — enforce only if the pre-fault JMAP probe succeeds,
#                    i.e. a mailbox is provisioned and the breaker path
#                    is actually reachable; otherwise report-only.
#   1              — always enforce (you have seeded a mailbox / set
#                    X-KMail-Dev-Stalwart-Account-Id via KMAIL_AUTH_*).
#   0              — never enforce (measure + report only).
# PREREQUISITE: the /jmap probe requires a provisioned Stalwart mailbox
# for the authenticated principal. With the dev-bypass token and no
# seeded mailbox the BFF returns 404 accountNotFound *before* touching a
# shard, so every iteration counts as an error regardless of shard
# health. See docs/LOADTEST.md.
ENFORCE="${KMAIL_CHAOS_SHARD_ENFORCE:-auto}"

# probe_jmap emits the HTTP status of one JMAP call (000 on no response).
probe_jmap() {
  curl -s -o /dev/null -w "%{http_code}" --max-time "$MAX_TIME" -X POST "$JMAP_URL/jmap" \
    -H "Authorization: Bearer $AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}' || true
}

echo "chaos-shard: pre-fault /readyz"
curl -fsS "$JMAP_URL/readyz" >/dev/null

# Decide enforceability from a healthy (pre-fault) JMAP probe: a 2xx/3xx
# means the mailbox prerequisite is met and shard fail-over is genuinely
# measurable; anything else (typically 404 accountNotFound) means it is
# not, so we fall back to report-only to avoid a false red.
if [ "$ENFORCE" = "auto" ]; then
  pre=$(probe_jmap); pre=${pre:-000}
  if [ "$pre" -ge 200 ] && [ "$pre" -lt 400 ]; then
    ENFORCE=1
  else
    ENFORCE=0
  fi
  echo "chaos-shard: pre-fault JMAP probe=${pre} -> enforce=${ENFORCE}"
fi

echo "chaos-shard: killing $SHARD_CONTAINER"
docker kill "$SHARD_CONTAINER" >/dev/null
# Always restart the shard, even if the SLO assertion below fails
# under `set -e`; otherwise a breach leaves the shard down.
trap 'docker start '"$SHARD_CONTAINER"' >/dev/null 2>&1 || true' EXIT

errs=0
for i in $(seq 1 "$ITERATIONS"); do
  if ! curl -fsS --max-time "$MAX_TIME" -X POST "$JMAP_URL/jmap" \
       -H "Authorization: Bearer $AUTH_TOKEN" \
       -H "Content-Type: application/json" \
       -d '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}' \
       >/dev/null 2>&1; then
    errs=$((errs+1))
  fi
done

echo "chaos-shard: restarting $SHARD_CONTAINER"
docker start "$SHARD_CONTAINER" >/dev/null
trap - EXIT
sleep 5
curl -fsS "$JMAP_URL/readyz" >/dev/null

ratio=$(awk -v errs="$errs" -v iter="$ITERATIONS" 'BEGIN{printf "%.4f", 100.0*errs/iter}')
echo "chaos-shard: errors=${errs}/${ITERATIONS} (${ratio}%) target<=${SLO_PCT}%"
if [ "$ENFORCE" = "1" ]; then
  echo "chaos-shard: enforcing SLO (mailbox prerequisite met)"
  awk -v ratio="$ratio" -v slo="$SLO_PCT" 'BEGIN{exit !(ratio+0 <= slo+0)}'
else
  echo "chaos-shard: report-only — no provisioned mailbox, so /jmap returns 404 before reaching a shard and the circuit-breaker fail-over is not exercised. Seed a mailbox or set KMAIL_CHAOS_SHARD_ENFORCE=1 to enforce. See docs/LOADTEST.md."
fi
