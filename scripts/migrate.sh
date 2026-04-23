#!/usr/bin/env bash
# KMail — local database migration runner.
#
# Waits for the compose Postgres to be healthy and applies every SQL
# file in `migrations/` (lexicographic order) against it. Each
# migration wraps its own transaction (see the `BEGIN; ... COMMIT;`
# bookends), so re-running this script is safe only if the migration
# files are themselves idempotent.
#
# Idempotence is enforced with a tiny `schema_migrations` bookkeeping
# table: each migration is recorded by filename after it applies, and
# subsequent runs skip anything already recorded.
#
# Usage:
#     ./scripts/migrate.sh           # run against the local compose stack
#     DATABASE_URL=... ./scripts/migrate.sh
#
# The script assumes the `docker compose` stack in `docker-compose.yml`
# is already up (`docker compose up -d postgres`). Override by setting
# `DATABASE_URL` to any `psql`-compatible connection string.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIGRATIONS_DIR="${REPO_ROOT}/migrations"

DATABASE_URL="${DATABASE_URL:-postgresql://kmail:kmail@localhost:5432/kmail}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-60}"

log() {
    printf '[migrate] %s\n' "$*"
}

require_psql() {
    if ! command -v psql >/dev/null 2>&1; then
        log "error: psql is not installed. Install postgresql-client or run via the postgres container."
        exit 1
    fi
}

wait_for_postgres() {
    log "waiting up to ${WAIT_TIMEOUT_SECONDS}s for ${DATABASE_URL%%\?*} to accept connections"
    local deadline=$(( $(date +%s) + WAIT_TIMEOUT_SECONDS ))
    while true; do
        if psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -Atqc 'SELECT 1' >/dev/null 2>&1; then
            log "postgres is reachable"
            return 0
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            log "error: postgres did not become reachable within ${WAIT_TIMEOUT_SECONDS}s"
            exit 1
        fi
        sleep 1
    done
}

ensure_bookkeeping_table() {
    psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -Atqc "
        CREATE TABLE IF NOT EXISTS schema_migrations (
            filename    TEXT PRIMARY KEY,
            applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
        );
    " >/dev/null
}

is_applied() {
    local filename="$1"
    local count
    count=$(psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -Atqc \
        "SELECT count(*) FROM schema_migrations WHERE filename = '${filename}'")
    [ "${count}" = "1" ]
}

record_applied() {
    local filename="$1"
    psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -Atqc \
        "INSERT INTO schema_migrations (filename) VALUES ('${filename}')
         ON CONFLICT (filename) DO NOTHING" >/dev/null
}

apply_migration() {
    local path="$1"
    local filename
    filename="$(basename "${path}")"
    if is_applied "${filename}"; then
        log "skip ${filename} (already applied)"
        return 0
    fi
    log "apply ${filename}"
    psql "${DATABASE_URL}" -v ON_ERROR_STOP=1 -f "${path}" >/dev/null
    record_applied "${filename}"
}

main() {
    require_psql
    if [ ! -d "${MIGRATIONS_DIR}" ]; then
        log "error: ${MIGRATIONS_DIR} does not exist"
        exit 1
    fi
    wait_for_postgres
    ensure_bookkeeping_table

    shopt -s nullglob
    local files=( "${MIGRATIONS_DIR}"/*.sql )
    if [ "${#files[@]}" -eq 0 ]; then
        log "no migration files in ${MIGRATIONS_DIR}"
        return 0
    fi

    IFS=$'\n' sorted_files=($(sort <<<"${files[*]}"))
    unset IFS

    for path in "${sorted_files[@]}"; do
        apply_migration "${path}"
    done

    log "migrations complete"
}

main "$@"
