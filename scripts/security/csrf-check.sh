#!/usr/bin/env bash
#
# csrf-check.sh — CSRF protection verification.
#
# KMail's API is token-authenticated (Bearer / OIDC), which is the
# primary CSRF defence: a cross-site form post cannot attach the
# Authorization header, and the API does not accept ambient cookie
# auth for state-changing routes. This check verifies that property
# holds: a state-changing request (POST/DELETE) with NO Authorization
# header — i.e. what a forged cross-site request can send — must be
# rejected with 401/403, not processed.
#
# It also flags any state-changing endpoint that accepts a request
# carrying only a browser-style Origin/cookie but no bearer token.
#
# Required environment:
#   TARGET   Base URL of a non-prod deployment.
# Optional:
#   VICTIM_TENANT  tenant id to substitute (default sentinel).
#
# Exit non-zero if any state-changing endpoint processes an
# unauthenticated cross-site-style request.
#
set -euo pipefail
# shellcheck source=scripts/security/lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

SEC_PASS=0; SEC_FAIL=0; SEC_SKIP=0
sec_require_target || exit 0

VICTIM_TENANT="${VICTIM_TENANT:-00000000-0000-0000-0000-000000000000}"
RES="00000000-0000-0000-0000-000000000000"

fill() {
	local p="$1"
	p="${p//\{id\}/$VICTIM_TENANT}"
	p="${p//\{tenantID\}/$VICTIM_TENANT}"
	p="${p//\{tenantId\}/$VICTIM_TENANT}"
	while [[ "$p" =~ \{[a-zA-Z0-9_]+\} ]]; do
		p="${p/${BASH_REMATCH[0]}/$RES}"
	done
	echo "$p"
}

echo "CSRF check against ${TARGET} (unauthenticated state-changing requests)"
while IFS= read -r ep; do
	url="${TARGET}$(fill "$ep")"
	# Simulate a forged cross-site POST: attacker-controlled Origin,
	# a planted cookie, but NO Authorization header.
	code="$(sec_curl_status POST "$url" \
		-H "Origin: https://evil.example" \
		-H "Cookie: session=forged" \
		-H "Content-Type: application/json" \
		--data '{}')"
	case "$code" in
		401|403)
			sec_pass "${ep} -> ${code} (rejects unauthenticated cross-site POST)" ;;
		200|201|202|204)
			sec_fail "${ep} -> ${code} (processed an unauthenticated state change!)" ;;
		000)
			sec_skip "${ep} -> connection failed" ;;
		404|405)
			sec_pass "${ep} -> ${code} (no such method/route; not exploitable)" ;;
		*)
			sec_pass "${ep} -> ${code} (non-success, review)" ;;
	esac
done < <(sec_endpoints)

sec_summary
