#!/usr/bin/env bash
# bench-caldav.sh — measures CalDAV event create latency against
# the local Stalwart CalDAV endpoint.
#
# Usage:
#   ./scripts/bench/bench-caldav.sh [N] [BASE_URL] [USER] [PASS] [CAL_PATH]
#
# Stalwart v0.16.0 keys /dav/cal/ by the account's *email*, not its
# bare login name (/dav/cal/kmail-dev/ 404s, /dav/cal/kmail-dev@kmail.dev/
# resolves), so CAL_PATH must use the full email segment.
#
# Example:
#   ./scripts/bench/bench-caldav.sh 50 http://localhost:8080 kmail-dev@kmail.dev <password> /dav/cal/kmail-dev@kmail.dev/default/

set -euo pipefail

N="${1:-50}"
BASE="${2:-http://localhost:8080}"
USER="${3:-kmail-dev@kmail.dev}"
PASS="${4:-kmail-dev}"
PATHP="${5:-/dav/cal/kmail-dev@kmail.dev/default/}"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

for i in $(seq 1 "$N"); do
    uid="bench-$(date +%s)-$$-$i@kmail.local"
    now=$(date -u +%Y%m%dT%H%M%SZ)
    # DTEND = now + 30 min. `date -d` is GNU-only; `date -r <epoch>`
    # is BSD / macOS. Compute the epoch, then format with a fallback
    # (mirrors the portable pattern in scripts/test-caldav.sh).
    end_epoch=$(( $(date +%s) + 1800 ))
    if ! dtend=$(date -u -d "@${end_epoch}" +%Y%m%dT%H%M%SZ 2>/dev/null); then
        dtend=$(date -u -r "${end_epoch}" +%Y%m%dT%H%M%SZ)
    fi
    ical=$(cat <<ICS
BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//kmail//bench//EN
BEGIN:VEVENT
UID:$uid
DTSTAMP:$now
DTSTART:$now
DTEND:$dtend
SUMMARY:Bench event $i
END:VEVENT
END:VCALENDAR
ICS
)
    # Measure latency with curl's own timer (`%{time_total}`,
    # seconds) rather than wrapping the call in `date +%s%N`: `%N`
    # is GNU-only (BSD/macOS `date` emits a literal "N", breaking
    # the arithmetic), and curl's timer is both portable and more
    # accurate since it excludes shell/spawn overhead.
    secs=$(curl -u "$USER:$PASS" -sS -o /dev/null -w '%{time_total}' \
        -X PUT "$BASE$PATHP$uid.ics" \
        -H "Content-Type: text/calendar" \
        --data-binary "$ical" || echo 0)
    awk -v s="$secs" 'BEGIN { printf "%d\n", (s * 1000) + 0.5 }' >>"$tmp"
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
