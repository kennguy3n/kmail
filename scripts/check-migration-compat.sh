#!/usr/bin/env bash
# KMail — migration backward-compatibility gate (WS4 Task 4).
#
# Zero-downtime deploys run the new code against the OLD schema for a
# window during a rolling rollout, and the OLD code against the NEW
# schema during rollback. A migration that drops/renames/retypes a
# column or table breaks one of those, so this check fails CI when a
# NEWLY ADDED up-migration contains a backward-incompatible statement.
#
# Scope: only up-migration files added relative to the base ref are
# scanned (`migrations/NNN_*.sql`, excluding `*.down.sql`). Down files
# are rollback-only and exempt. Existing migrations are never re-judged.
#
# Escape hatch: a migration that is intentionally breaking (e.g. the
# tail end of a multi-release deprecation) may include the marker
#     -- kmail:allow-breaking
# anywhere in the file to acknowledge the risk and pass the gate.
#
# Usage:
#     ./scripts/check-migration-compat.sh            # vs origin/main
#     BASE_REF=origin/main ./scripts/check-migration-compat.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

BASE_REF="${BASE_REF:-origin/main}"
ALLOW_MARKER="kmail:allow-breaking"

# Patterns that break new-code-vs-old-schema or old-code-vs-new-schema.
# Case-insensitive, whitespace-tolerant.
PATTERNS=(
    'DROP[[:space:]]+COLUMN'
    'DROP[[:space:]]+TABLE'
    'RENAME[[:space:]]+COLUMN'
    'RENAME[[:space:]]+TO'
    'ALTER[[:space:]]+COLUMN[[:space:]]+.*[[:space:]]+TYPE'
    'DROP[[:space:]]+NOT[[:space:]]+NULL' # ok-ish, but kept conservative
)

log() { printf '[migration-compat] %s\n' "$*" >&2; }

# Resolve the set of added up-migration files. Prefer git diff against
# the base ref; fall back to scanning all when git history is absent.
changed_files() {
    if git rev-parse --verify "${BASE_REF}" >/dev/null 2>&1; then
        # Two-dot diff (BASE_REF vs working tree) so both committed and
        # staged additions are caught — useful as a local pre-push
        # check as well as in CI. Untracked-but-new files are appended
        # so a freshly written migration is checked before it is even
        # `git add`ed.
        git diff --name-only --diff-filter=A "${BASE_REF}" -- 'migrations/*.sql' 2>/dev/null || true
        git ls-files --others --exclude-standard -- 'migrations/*.sql' 2>/dev/null || true
    else
        log "base ref ${BASE_REF} not found; scanning all migrations"
        git ls-files 'migrations/*.sql' 2>/dev/null || true
    fi
}

violations=0
while IFS= read -r f; do
    [ -z "${f}" ] && continue
    case "${f}" in
        *.down.sql) continue ;;  # rollback files are exempt
    esac
    [ -f "${f}" ] || continue

    if grep -qi "${ALLOW_MARKER}" "${f}"; then
        log "skip ${f} (marked ${ALLOW_MARKER})"
        continue
    fi

    for pat in "${PATTERNS[@]}"; do
        if grep -Eiqn "${pat}" "${f}"; then
            log "BREAKING: ${f} matches /${pat}/"
            grep -Ein "${pat}" "${f}" | sed 's/^/[migration-compat]     /' >&2
            violations=$((violations + 1))
        fi
    done
done < <(changed_files)

if [ "${violations}" -gt 0 ]; then
    log "found ${violations} backward-incompatible statement(s)."
    log "Fix by splitting into a deprecation sequence, or add the"
    log "'-- ${ALLOW_MARKER}' marker if this is an intentional final step."
    exit 1
fi

log "no backward-incompatible migrations detected"
