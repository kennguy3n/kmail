#!/usr/bin/env bash
# scripts/loadtest/chaos-during-load.sh — inject chaos *while* the
# scale load test is in steady state and measure the impact on
# throughput and error rate.
#
# Unlike the standalone chaos-*.sh scripts (which drive a small
# synthetic probe loop), this orchestrator runs the full weighted
# workload from scale-5k.go against the seeded tenant fleet, waits
# for steady state, then fires the existing chaos injectors one at a
# time. Afterwards it renders the Markdown report and prints an
# impact summary comparing the chaos window against the steady-state
# baseline (buckets recorded by scale-5k.go).
#
# Flow:
#   1. Launch scale-5k.go in the background (writes a JSON summary).
#   2. Sleep through the ramp-up so chaos lands during steady state.
#   3. Run each enabled chaos script in turn, recording the wall-
#      clock window (offset from load start) that it covered.
#   4. Wait for the load run to finish.
#   5. Render the report and compute baseline-vs-chaos impact.
#
# Inputs (env, all with compose-stack defaults):
#   KMAIL_API_URL          BFF base URL              (http://localhost:8088)
#   KMAIL_DEV_BEARER       dev bearer token          (kmail-dev)
#   CHAOS_LOAD_TENANTS     tenants to drive load on  (50)
#   CHAOS_LOAD_WORKERS     peak workers              (32)
#   CHAOS_RAMPUP           ramp-up duration          (30s)
#   CHAOS_STEADY           steady duration           (3m)
#   CHAOS_COOLDOWN         cool-down duration        (15s)
#   CHAOS_SCRIPTS          space-separated injectors (chaos-shard.sh chaos-postgres.sh chaos-valkey.sh)
#   CHAOS_OUT_DIR          report output directory   (./loadtest-out)
#
# Flags:
#   --dry-run   print the plan and exit (no load, no docker)
#   --help      show usage
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../.." && pwd)"

API_URL="${KMAIL_API_URL:-http://localhost:8088}"
BEARER="${KMAIL_DEV_BEARER:-kmail-dev}"
LOAD_TENANTS="${CHAOS_LOAD_TENANTS:-50}"
LOAD_WORKERS="${CHAOS_LOAD_WORKERS:-32}"
RAMPUP="${CHAOS_RAMPUP:-30s}"
STEADY="${CHAOS_STEADY:-3m}"
COOLDOWN="${CHAOS_COOLDOWN:-15s}"
SCRIPTS="${CHAOS_SCRIPTS:-chaos-shard.sh chaos-postgres.sh chaos-valkey.sh}"
OUT_DIR="${CHAOS_OUT_DIR:-${REPO_ROOT}/loadtest-out}"

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help)
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "chaos-during-load: unknown argument: $arg" >&2
      exit 2
      ;;
  esac
done

# dur_to_secs converts a Go-style duration (e.g. 30s, 3m, 1m30s) to
# whole seconds so the script can sleep/compare with awk.
dur_to_secs() {
  awk -v s="$1" 'BEGIN{
    total=0; num="";
    for (i=1; i<=length(s); i++) {
      c=substr(s,i,1);
      if (c ~ /[0-9.]/) { num=num c; }
      else {
        if (c=="h") total+=num*3600;
        else if (c=="m") total+=num*60;
        else if (c=="s") total+=num;
        num="";
      }
    }
    printf "%d", total;
  }'
}

RAMPUP_S="$(dur_to_secs "$RAMPUP")"
STEADY_S="$(dur_to_secs "$STEADY")"
COOLDOWN_S="$(dur_to_secs "$COOLDOWN")"
TOTAL_S=$((RAMPUP_S + STEADY_S + COOLDOWN_S))

JSON_OUT="${OUT_DIR}/scale-report.json"
MD_OUT="${OUT_DIR}/scale-report.md"

print_plan() {
  echo "chaos-during-load: plan"
  echo "  BFF URL        : ${API_URL}"
  echo "  load tenants   : ${LOAD_TENANTS}"
  echo "  peak workers   : ${LOAD_WORKERS}"
  echo "  phases         : ramp ${RAMPUP_S}s -> steady ${STEADY_S}s -> cooldown ${COOLDOWN_S}s (total ${TOTAL_S}s)"
  echo "  chaos scripts  : ${SCRIPTS}"
  echo "  output dir     : ${OUT_DIR}"
  echo "  json/md        : ${JSON_OUT} / ${MD_OUT}"
}

if [ "$DRY_RUN" -eq 1 ]; then
  print_plan
  echo "chaos-during-load: DRY RUN — no load generated, no chaos injected"
  # Sanity-check that every requested injector exists and is runnable.
  for s in $SCRIPTS; do
    if [ -x "${HERE}/${s}" ]; then
      echo "  ok: ${HERE}/${s}"
    else
      echo "  MISSING/!x: ${HERE}/${s}" >&2
    fi
  done
  exit 0
fi

mkdir -p "$OUT_DIR"
print_plan

# Verify the BFF is reachable before committing to a long run.
if ! curl -fsS "${API_URL}/readyz" >/dev/null 2>&1; then
  echo "chaos-during-load: BFF not reachable at ${API_URL}/readyz" >&2
  exit 1
fi

echo "chaos-during-load: launching scale-5k load generator in background"
(
  cd "$REPO_ROOT"
  go run ./scripts/loadtest/scale-5k.go \
    --api-url "$API_URL" --auth-token "$BEARER" \
    --tenants "$LOAD_TENANTS" --workers "$LOAD_WORKERS" \
    --rampup "$RAMPUP" --steady "$STEADY" --cooldown "$COOLDOWN" \
    --json-out "$JSON_OUT"
) &
LOAD_PID=$!
LOAD_START="$(date +%s)"

# Ensure the load generator is torn down if we are interrupted.
cleanup() {
  if kill -0 "$LOAD_PID" 2>/dev/null; then
    kill "$LOAD_PID" 2>/dev/null || true
    wait "$LOAD_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

echo "chaos-during-load: waiting ${RAMPUP_S}s for steady state"
sleep "$RAMPUP_S"

# Record the chaos window as an offset (seconds) from load start so
# it can be correlated with scale-5k's throughput buckets.
CHAOS_START_OFF=$(( $(date +%s) - LOAD_START ))
for s in $SCRIPTS; do
  script_path="${HERE}/${s}"
  if [ ! -x "$script_path" ]; then
    echo "chaos-during-load: skipping missing/non-exec injector ${script_path}" >&2
    continue
  fi
  echo "chaos-during-load: injecting ${s} (t+$(( $(date +%s) - LOAD_START ))s)"
  if "$script_path"; then
    echo "chaos-during-load: ${s} recovered within SLO"
  else
    echo "chaos-during-load: ${s} reported an SLO breach (rc=$?)" >&2
  fi
done
CHAOS_END_OFF=$(( $(date +%s) - LOAD_START ))

echo "chaos-during-load: waiting for load run to finish"
# Capture the load generator's exit code without letting `set -e` abort here:
# if it died (e.g. chaos disrupted the generator itself, OOM, signal) we still
# want to render the report and impact summary from whatever it managed to
# write, then surface the failure at the very end.
LOAD_RC=0
wait "$LOAD_PID" || LOAD_RC=$?
trap - EXIT INT TERM
if [ "$LOAD_RC" -ne 0 ]; then
  echo "chaos-during-load: load generator exited non-zero (rc=${LOAD_RC}) — rendering from any partial summary" >&2
fi

if [ ! -f "$JSON_OUT" ]; then
  echo "chaos-during-load: load run produced no summary at ${JSON_OUT}" >&2
  exit 1
fi

echo "chaos-during-load: rendering report"
( cd "$REPO_ROOT" && go run ./scripts/loadtest/report.go --in "$JSON_OUT" --out "$MD_OUT" --fail-on-violation=false )

# Impact summary: compare buckets inside the chaos window against
# steady-state baseline buckets outside it. Requires jq.
if command -v jq >/dev/null 2>&1; then
  echo "chaos-during-load: impact summary (chaos window t+${CHAOS_START_OFF}s..t+${CHAOS_END_OFF}s)"
  jq -r --argjson lo "$CHAOS_START_OFF" --argjson hi "$CHAOS_END_OFF" '
    .buckets as $b
    | ($b | map(select(.phase=="steady" and (.start_s < $lo or .start_s > $hi)))) as $base
    | ($b | map(select(.start_s >= $lo and .start_s <= $hi))) as $chaos
    | def avg(f): if (length>0) then (map(f)|add)/length else 0 end;
      def sum(f): (map(f)|add) // 0;
      "  baseline : \($base|avg(.rps)|.*10|round/10) req/s, \($base|sum(.errors)) errors over \($base|length) buckets",
      "  chaos    : \($chaos|avg(.rps)|.*10|round/10) req/s, \($chaos|sum(.errors)) errors over \($chaos|length) buckets"
  ' "$JSON_OUT" || echo "  (impact computation failed)"
else
  echo "chaos-during-load: jq not found — skipping impact summary (report at ${MD_OUT})"
fi

echo "chaos-during-load: done — report at ${MD_OUT}"

# Propagate a load-generator failure so callers/CI still see it, now that the
# report has been produced.
if [ "$LOAD_RC" -ne 0 ]; then
  exit "$LOAD_RC"
fi
