#!/usr/bin/env bash
# bench-caldav.sh — measures CalDAV event create latency against
# the local Stalwart CalDAV endpoint.
#
# Usage:
#   ./scripts/bench/bench-caldav.sh [N] [BASE_URL] [USER] [PASS] [CAL_PATH]
#
# Example:
#   ./scripts/bench/bench-caldav.sh 50 http://localhost:8080 dev kmail-dev /dav/calendars/dev/default/

set -euo pipefail

N="${1:-50}"
BASE="${2:-http://localhost:8080}"
USER="${3:-dev}"
PASS="${4:-kmail-dev}"
PATHP="${5:-/dav/calendars/dev/default/}"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

for i in $(seq 1 "$N"); do
    uid="bench-$(date +%s%N)-$i@kmail.local"
    ical=$(cat <<ICS
BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//kmail//bench//EN
BEGIN:VEVENT
UID:$uid
DTSTAMP:$(date -u +%Y%m%dT%H%M%SZ)
DTSTART:$(date -u +%Y%m%dT%H%M%SZ)
DTEND:$(date -u -d '+30 minutes' +%Y%m%dT%H%M%SZ)
SUMMARY:Bench event $i
END:VEVENT
END:VCALENDAR
ICS
)
    start=$(date +%s%N)
    curl -u "$USER:$PASS" -sS -o /dev/null \
        -X PUT "$BASE$PATHP$uid.ics" \
        -H "Content-Type: text/calendar" \
        --data-binary "$ical" || true
    end=$(date +%s%N)
    echo "$(( (end - start) / 1000000 ))" >>"$tmp"
done

python3 - "$tmp" <<'PY'
import json, statistics, sys
nums = sorted(int(x) for x in open(sys.argv[1]))
def pct(p): return nums[min(int(len(nums)*p), len(nums)-1)]
out = {
    "n": len(nums),
    "p50_ms": pct(0.50),
    "p95_ms": pct(0.95),
    "p99_ms": pct(0.99),
    "max_ms": nums[-1],
    "mean_ms": round(statistics.mean(nums), 1),
}
print("\nCalDAV PUT latency (ms):")
print(f"  N      : {out['n']}")
print(f"  P50    : {out['p50_ms']}")
print(f"  P95    : {out['p95_ms']}")
print(f"  P99    : {out['p99_ms']}")
print(f"  max    : {out['max_ms']}")
sys.stderr.write(json.dumps(out) + "\n")
PY
