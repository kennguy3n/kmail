#!/usr/bin/env bash
#
# sqli-scan.sh — SQL-injection probe against tenant-scoped endpoints.
#
# For each endpoint it injects a curated payload set into the path
# parameter and asserts the server neither 500s nor leaks a database
# error string. KMail uses parameterised queries + RLS throughout, so
# the expected outcome is a clean 4xx (validation/not-found), never a
# 500 or an SQL error in the body.
#
# Required environment:
#   TARGET   Base URL of a non-prod deployment.
# Optional:
#   TOKEN    Bearer token (some endpoints require auth before they
#            reach the query layer).
#
# Exit non-zero if any payload triggers a 500 or a DB-error leak.
#
set -euo pipefail
# shellcheck source=scripts/security/lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

SEC_PASS=0; SEC_FAIL=0; SEC_SKIP=0
sec_require_target || exit 0

AUTH=()
[ -n "${TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${TOKEN}")

# Classic injection payloads (URL-encoded where needed). The goal is
# to break out of a string/identifier context if one were ever built
# by concatenation.
PAYLOADS=(
	"1'"
	"1';--"
	"1%20OR%201=1"
	"1)%20OR%20(1=1"
	"'%20UNION%20SELECT%20NULL--"
	"1;DROP%20TABLE%20users--"
	"%27%3B%20SELECT%20pg_sleep%285%29%3B--"
)

# Markers that should NEVER appear in a response body — they indicate
# a raw driver/SQL error escaped to the client.
LEAK_MARKERS='(pq:|pgx|syntax error at or near|SQLSTATE|relation ".*" does not exist|unterminated quoted string)'

inject_path() {
	# Replace the tenant slot and every other {…} with the payload.
	local p="$1" payload="$2"
	p="${p//\{id\}/$payload}"
	p="${p//\{tenantID\}/$payload}"
	p="${p//\{tenantId\}/$payload}"
	while [[ "$p" =~ \{[a-zA-Z0-9_]+\} ]]; do
		p="${p/${BASH_REMATCH[0]}/$payload}"
	done
	echo "$p"
}

echo "SQLi scan against ${TARGET} (${#PAYLOADS[@]} payloads/endpoint)"
while IFS= read -r ep; do
	for payload in "${PAYLOADS[@]}"; do
		url="${TARGET}$(inject_path "$ep" "$payload")"
		body="$(sec_curl_body GET "$url" "${AUTH[@]}")"
		code="$(sec_curl_status GET "$url" "${AUTH[@]}")"
		if [ "$code" = "500" ]; then
			sec_fail "${ep} payload[${payload}] -> 500"
		elif echo "$body" | grep -qiE "$LEAK_MARKERS"; then
			sec_fail "${ep} payload[${payload}] -> DB error leaked in body"
		elif [ "$code" = "000" ]; then
			sec_skip "${ep} payload[${payload}] -> connection failed"
		else
			sec_pass "${ep} payload[${payload}] -> ${code}"
		fi
	done
done < <(sec_endpoints)

sec_summary
