#!/usr/bin/env bash
# KMail — database migration runner (WS4 Task 4).
#
# Thin wrapper around the `kmail-migrate` Go runner
# (internal/schemamigrate). The Go runner provides what the old bare
# psql loop could not:
#
#   - Postgres ADVISORY LOCKING so two concurrent deploys can't apply
#     the same migration twice (zero-downtime rolling deploys),
#   - ROLLBACK via optional `migrations/NNN_*.down.sql` companions,
#   - the same filename-keyed `schema_migrations` bookkeeping table as
#     before, so an already-migrated database keeps working.
#
# Usage:
#     ./scripts/migrate.sh                 # apply all pending (up)
#     ./scripts/migrate.sh up
#     ./scripts/migrate.sh down [N]        # roll back last N (default 1)
#     ./scripts/migrate.sh status
#     DATABASE_URL=... ./scripts/migrate.sh up
#
# DATABASE_URL defaults to the local compose stack. Bring Postgres up
# first (`docker compose up -d postgres`).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="${REPO_ROOT}/migrations"

export DATABASE_URL="${DATABASE_URL:-postgresql://kmail:kmail@localhost:5432/kmail}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-60}"

log() { printf '[migrate] %s\n' "$*" >&2; }

wait_for_postgres() {
    log "waiting up to ${WAIT_TIMEOUT_SECONDS}s for postgres"
    local deadline=$(( $(date +%s) + WAIT_TIMEOUT_SECONDS ))
    while true; do
        if command -v psql >/dev/null 2>&1; then
            if psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -Atqc 'SELECT 1' >/dev/null 2>&1; then
                log "postgres is reachable"
                return 0
            fi
        else
            # No psql available — let the Go runner's own connect
            # timeout handle reachability instead of blocking here.
            return 0
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            log "error: postgres did not become reachable within ${WAIT_TIMEOUT_SECONDS}s"
            exit 1
        fi
        sleep 1
    done
}

main() {
    if [ ! -d "${MIGRATIONS_DIR}" ]; then
        log "error: ${MIGRATIONS_DIR} does not exist"
        exit 1
    fi
    wait_for_postgres

    # Default subcommand is `up` so existing callers (CI, compose
    # entrypoints) that invoke `migrate.sh` with no args keep working.
    local subcmd="${1:-up}"
    shift || true

    log "running: kmail-migrate ${subcmd} $*"
    ( cd "${REPO_ROOT}" && go run ./cmd/kmail-migrate -dir "${MIGRATIONS_DIR}" "${subcmd}" "$@" )
}

main "$@"
