#!/usr/bin/env bash
#
# lib.sh — shared helpers for the security test harness. Sourced by
# the individual check scripts; not executed directly.
#
# Common environment:
#   TARGET   Base URL of the API under test (e.g. https://staging.kmail.example)
#
# The checks are written to run against a *non-production* deployment
# (staging / ephemeral). They never fabricate credentials; tokens are
# supplied by the operator via environment variables.

# sec_require_target aborts unless $TARGET is set. Checks call this
# first so an unconfigured run exits 0-with-a-note in the caller,
# rather than firing requests at nothing.
sec_require_target() {
	if [ -z "${TARGET:-}" ]; then
		echo "SKIPPED: set TARGET to the base URL of a non-prod deployment" >&2
		return 1
	fi
	return 0
}

# sec_curl_status METHOD URL [extra curl args...] -> prints HTTP status code.
# -s silent, -o /dev/null discard body, -w status only. A connection
# failure prints 000 (curl's convention) so callers can distinguish
# "server said X" from "could not connect".
sec_curl_status() {
	local method="$1" url="$2"
	shift 2
	curl -k -s -o /dev/null -w '%{http_code}' -X "$method" "$@" "$url" 2>/dev/null || echo "000"
}

# sec_curl_body METHOD URL [extra curl args...] -> prints response body.
sec_curl_body() {
	local method="$1" url="$2"
	shift 2
	curl -k -s -X "$method" "$@" "$url" 2>/dev/null || true
}

# sec_pass / sec_fail / sec_skip — uniform result lines + counters.
# Callers must initialise SEC_PASS/SEC_FAIL/SEC_SKIP to 0.
sec_pass() { SEC_PASS=$((SEC_PASS + 1)); echo "PASS: $*"; }
sec_fail() { SEC_FAIL=$((SEC_FAIL + 1)); echo "FAIL: $*"; }
sec_skip() { SEC_SKIP=$((SEC_SKIP + 1)); echo "SKIP: $*"; }

# sec_summary prints a tally and returns non-zero if any check failed,
# so the script is CI-gateable.
sec_summary() {
	echo "---"
	echo "pass=${SEC_PASS:-0} fail=${SEC_FAIL:-0} skip=${SEC_SKIP:-0}"
	[ "${SEC_FAIL:-0}" -eq 0 ]
}

# sec_endpoints prints the non-comment lines of tenant-endpoints.txt.
sec_endpoints() {
	local f
	f="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/tenant-endpoints.txt"
	grep -vE '^\s*#' "$f" | grep -vE '^\s*$'
}
