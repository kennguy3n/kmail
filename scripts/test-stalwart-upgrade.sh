#!/usr/bin/env sh
# KMail — Stalwart upgrade compatibility test.
#
# Boots Stalwart v0.16.0 (the current pin) and v1.0.0 (the
# upcoming release) sequentially against the rest of the local
# compose stack and runs `scripts/test-e2e.sh` against each. The
# point is to exercise the JMAP compatibility shim
# (`internal/jmap/compat.go`) on both shapes so a regression on
# either version is caught before the production pin moves.
#
# Inputs:
#   STALWART_V0_IMAGE  — defaults to stalwartlabs/stalwart:v0.16.0
#   STALWART_V1_IMAGE  — defaults to stalwartlabs/stalwart:v1.0.0
#                       (override to a release-candidate image
#                        until v1.0.0 publishes a stable tag).
#   COMPOSE_PROJECT    — defaults to kmail-upgrade-test
#   KMAIL_API_URL      — defaults to http://localhost:8080
#
# Exit code:
#   0  — both versions passed e2e.
#   1  — one or more versions failed.
#
# This script never runs in CI by default — operators invoke it
# from the host before staging an upgrade. See
# `docs/STALWART_UPGRADE.md` for the manual rollout plan it
# supports.

set -u

V0_IMAGE="${STALWART_V0_IMAGE:-stalwartlabs/stalwart:v0.16.0}"
V1_IMAGE="${STALWART_V1_IMAGE:-stalwartlabs/stalwart:v1.0.0}"
PROJECT="${COMPOSE_PROJECT:-kmail-upgrade-test}"
API_URL="${KMAIL_API_URL:-http://localhost:8080}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

step() { printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

run_for_version() {
  IMAGE="$1"
  TAG_LABEL="$2"
  step "starting $TAG_LABEL ($IMAGE)"
  STALWART_IMAGE="$IMAGE" \
    docker compose -p "$PROJECT-$TAG_LABEL" \
                   -f "$REPO_ROOT/docker-compose.yml" \
                   up -d
  echo "[$TAG_LABEL] waiting up to 90s for kmail-api ready"
  i=0
  while :; do
    if curl --fail --silent "$API_URL/readyz" >/dev/null 2>&1; then
      break
    fi
    i=$((i + 1))
    if [ $i -gt 90 ]; then
      echo "[$TAG_LABEL] kmail-api never became ready" >&2
      docker compose -p "$PROJECT-$TAG_LABEL" -f "$REPO_ROOT/docker-compose.yml" logs --tail=200 stalwart kmail-api >&2
      docker compose -p "$PROJECT-$TAG_LABEL" -f "$REPO_ROOT/docker-compose.yml" down -v
      return 1
    fi
    sleep 1
  done
  echo "[$TAG_LABEL] running e2e suite"
  if KMAIL_API_URL="$API_URL" sh "$SCRIPT_DIR/test-e2e.sh"; then
    RESULT=0
  else
    RESULT=$?
  fi
  docker compose -p "$PROJECT-$TAG_LABEL" -f "$REPO_ROOT/docker-compose.yml" down -v
  return "$RESULT"
}

EXIT_CODE=0
if ! run_for_version "$V0_IMAGE" "v0"; then
  EXIT_CODE=1
fi
if ! run_for_version "$V1_IMAGE" "v1"; then
  EXIT_CODE=1
fi

step "stalwart upgrade test exit=$EXIT_CODE"
exit "$EXIT_CODE"
