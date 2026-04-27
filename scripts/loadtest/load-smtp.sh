#!/usr/bin/env bash
# scripts/loadtest/load-smtp.sh — sustained SMTP submission load.
#
# Runs `swaks` in a loop at a fixed TPS against the local Stalwart
# submission port. Used as the SMTP companion to load-jmap.go for
# the Phase 7 mixed-workload baseline.
#
# Usage:
#   scripts/loadtest/load-smtp.sh [TPS] [DURATION_S]
#
# Defaults: 25 messages/sec, 60 seconds.
set -euo pipefail

TPS="${1:-25}"
DURATION_S="${2:-60}"
SMTP_HOST="${KMAIL_SMTP_HOST:-localhost}"
SMTP_PORT="${KMAIL_SMTP_PORT:-1025}"
SMTP_FROM="${KMAIL_SMTP_FROM:-loadtest@kmail.dev}"
SMTP_TO="${KMAIL_SMTP_TO:-inbox@kmail.dev}"

if ! command -v swaks >/dev/null 2>&1; then
  echo "load-smtp: swaks not on PATH (apt-get install swaks)"
  exit 1
fi

interval_us=$(awk -v tps="$TPS" 'BEGIN{printf "%d", 1000000.0/tps}')
end_at=$(( $(date +%s) + DURATION_S ))
sent=0
errs=0

echo "load-smtp: ${TPS} TPS for ${DURATION_S}s against ${SMTP_HOST}:${SMTP_PORT}"
while [ "$(date +%s)" -lt "$end_at" ]; do
  swaks --to "$SMTP_TO" --from "$SMTP_FROM" \
        --server "$SMTP_HOST" --port "$SMTP_PORT" \
        --header "Subject: kmail-loadtest" \
        --body "loadtest at $(date -u +%FT%TZ)" \
        --silent 2 >/dev/null 2>&1 \
    && sent=$((sent+1)) \
    || errs=$((errs+1))
  usleep "$interval_us" 2>/dev/null || sleep 0.04
done

echo "load-smtp: sent=${sent} errors=${errs}"
