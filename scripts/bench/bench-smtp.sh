#!/usr/bin/env bash
# bench-smtp.sh — measures send-accepted latency (SMTP DATA → 250 OK)
# for N messages against the local Stalwart submission port.
#
# Usage:
#   ./scripts/bench/bench-smtp.sh [N] [SMTP_HOST:PORT] [FROM] [TO]
#
# Example:
#   ./scripts/bench/bench-smtp.sh 100 localhost:587 bench@kmail.local dev@kmail.local
#
# Computes P50/P95/P99 in milliseconds and prints a human-readable
# summary plus a JSON blob on stderr.
#
# Requires `swaks` on $PATH (apt: swaks).

set -euo pipefail

N="${1:-50}"
HOSTPORT="${2:-localhost:587}"
FROM="${3:-bench@kmail.local}"
TO="${4:-dev@kmail.local}"

if ! command -v swaks >/dev/null; then
    echo "bench-smtp: swaks is required (apt-get install swaks)" >&2
    exit 2
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

for i in $(seq 1 "$N"); do
    start=$(date +%s%N)
    swaks --quit-after DATA-OK \
        --server "$HOSTPORT" --from "$FROM" --to "$TO" \
        --header "Subject: bench $i" \
        --body "bench $i $(date -u +%FT%TZ)" \
        --auth-user dev --auth-password kmail-dev \
        >/dev/null 2>&1 || true
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
print("\nSMTP send-accepted latency (ms):")
print(f"  N      : {out['n']}")
print(f"  P50    : {out['p50_ms']}")
print(f"  P95    : {out['p95_ms']}")
print(f"  P99    : {out['p99_ms']}")
print(f"  max    : {out['max_ms']}")
print(f"  mean   : {out['mean_ms']}")
sys.stderr.write(json.dumps(out) + "\n")
PY
