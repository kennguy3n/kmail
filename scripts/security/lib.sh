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

# sec_curl_full METHOD URL [extra curl args...] -> prints the response
# body followed by a final line "SEC_HTTP_STATUS:<code>", captured from
# a SINGLE request so the body and status are guaranteed to describe the
# same response (issuing two requests can race — rate limiting, a
# transient 5xx, or recovery between calls would correlate a body with
# the wrong status). A connection failure yields status 000 with an
# empty body. Callers split with:
#   resp="$(sec_curl_full GET "$url" ...)"
#   code="${resp##*SEC_HTTP_STATUS:}"
#   body="${resp%$'\n'SEC_HTTP_STATUS:*}"
sec_curl_full() {
	local method="$1" url="$2" out
	shift 2
	# -w appends the status after the body in the SAME request. On a
	# connection failure curl exits non-zero; normalise to a single
	# 000 marker so the output always has exactly one status marker.
	out="$(curl -k -s -w $'\nSEC_HTTP_STATUS:%{http_code}' -X "$method" "$@" "$url" 2>/dev/null)" || out=$'\nSEC_HTTP_STATUS:000'
	printf '%s' "$out"
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
