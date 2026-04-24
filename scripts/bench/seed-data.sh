#!/usr/bin/env bash
# seed-data.sh — seeds the local JMAP inbox with N synthetic
# messages so the benchmark harness operates on realistic data.
#
# Usage:
#   ./scripts/bench/seed-data.sh [N] [JMAP_URL] [AUTH_TOKEN] [ACCOUNT_ID]
#
# Example:
#   ./scripts/bench/seed-data.sh 1000 http://localhost:8080 kmail-dev dev

set -euo pipefail

N="${1:-1000}"
JMAP_URL="${2:-http://localhost:8080}"
TOKEN="${3:-kmail-dev}"
ACCOUNT_ID="${4:-dev}"

echo "Seeding $N messages into account $ACCOUNT_ID …"
for i in $(seq 1 "$N"); do
    payload=$(cat <<JSON
{
  "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
  "methodCalls": [
    ["Email/set", {
      "accountId": "$ACCOUNT_ID",
      "create": {
        "seed$i": {
          "mailboxIds": {"inbox": true},
          "keywords": {"\$seen": true},
          "from": [{"email": "bench@kmail.local", "name": "Bench"}],
          "to": [{"email": "dev@kmail.local"}],
          "subject": "Bench seed $i",
          "receivedAt": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
          "bodyValues": {"text": {"value": "Bench seed body $i"}},
          "textBody": [{"partId": "text", "type": "text/plain"}]
        }
      }
    }, "c$i"]
  ]
}
JSON
)
    curl -sS -o /dev/null \
        -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d "$payload" \
        "$JMAP_URL/jmap" || true
    if (( i % 100 == 0 )); then
        echo "  seeded $i / $N"
    fi
done
echo "Done."
