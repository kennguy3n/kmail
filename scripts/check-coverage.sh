#!/usr/bin/env bash
# KMail — Go test-coverage gate (WS4 Task 5).
#
# Runs the Go test suite with a coverage profile and fails if total
# statement coverage (over packages that have tests) is below
# MIN_COVERAGE. The floor ratchets UP over time — never down. It has
# now reached the WS4 destination of 80%: every tested package and the
# global total sit above that line. The default below matches the gate
# enforced in CI (.github/workflows/ci.yml).
#
# Usage:
#     ./scripts/check-coverage.sh
#     MIN_COVERAGE=70 ./scripts/check-coverage.sh
#
# Writes coverage.out (consumed by `go tool cover -html`) to the repo
# root so it can be uploaded as a CI artifact / badge source.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

MIN_COVERAGE="${MIN_COVERAGE:-80}"
PROFILE="${COVERAGE_PROFILE:-coverage.out}"

log() { printf '[coverage] %s\n' "$*" >&2; }

# Package set: packages that actually contain tests, EXCLUDING vendored
# JS deps that happen to ship Go source (web/node_modules/**). Scoping to
# tested packages keeps the coverage denominator honest (untested
# packages would otherwise drag the number toward zero) AND sidesteps a
# managed-toolchain quirk where instrumenting a test-less package invokes
# the `covdata` merge tool, which the pinned toolchain omits.
mapfile -t PKGS < <(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v '/web/node_modules/')

if [ "${#PKGS[@]}" -eq 0 ]; then
    log "error: no packages with tests found"
    exit 1
fi

log "running go test with coverage (min ${MIN_COVERAGE}%) over ${#PKGS[@]} tested packages"
# -covermode=atomic is required alongside -race. Coverage is measured
# per-package against its own statements.
go test -race -covermode=atomic -coverprofile="${PROFILE}" "${PKGS[@]}"

total_line="$(go tool cover -func="${PROFILE}" | tail -1)"
# Last line looks like: "total:	(statements)	63.4%"
total_pct="$(printf '%s' "${total_line}" | grep -Eo '[0-9]+\.[0-9]+' | tail -1)"
if [ -z "${total_pct}" ]; then
    log "error: could not parse total coverage from: ${total_line}"
    exit 1
fi

log "total coverage: ${total_pct}% (threshold ${MIN_COVERAGE}%)"

# Float compare via awk (POSIX shells lack float arithmetic).
if awk -v have="${total_pct}" -v want="${MIN_COVERAGE}" 'BEGIN { exit (have >= want) ? 0 : 1 }'; then
    log "coverage gate passed"
else
    log "coverage ${total_pct}% is below the ${MIN_COVERAGE}% threshold"
    exit 1
fi
