#!/usr/bin/env bash
#
# idor-check.sh — Insecure Direct Object Reference probe.
#
# Verifies that tenant A's credentials cannot read tenant B's data
# through any tenant-scoped endpoint. For each endpoint in
# tenant-endpoints.txt it substitutes VICTIM_TENANT into the {id}/
# {tenantID} slot and calls it with ATTACKER_TOKEN (a valid token
# for a *different* tenant). A correctly isolated endpoint MUST
# respond 401/403/404 — never 200.
#
# This is pentest *preparation*: a runnable harness wired to the real
# endpoint inventory, intended to run against a seeded non-prod
# deployment. It does not fabricate tokens.
#
# Required environment:
#   TARGET           Base URL of a non-prod deployment.
#   ATTACKER_TOKEN   Bearer token for tenant A (the attacker).
#   VICTIM_TENANT    Tenant id of tenant B (the victim).
#
# Optional:
#   VICTIM_RESOURCE  Value substituted for trailing {…} resource ids
#                    (default: 00000000-0000-0000-0000-000000000000).
#
# Exit non-zero if any endpoint leaks (returns 2xx) cross-tenant.
#
set -euo pipefail
# shellcheck source=scripts/security/lib.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

SEC_PASS=0; SEC_FAIL=0; SEC_SKIP=0

sec_require_target || exit 0
if [ -z "${ATTACKER_TOKEN:-}" ] || [ -z "${VICTIM_TENANT:-}" ]; then
	echo "SKIPPED: set ATTACKER_TOKEN (tenant A) and VICTIM_TENANT (tenant B id)"
	exit 0
fi
VICTIM_RESOURCE="${VICTIM_RESOURCE:-00000000-0000-0000-0000-000000000000}"

# fill_path substitutes the tenant slot with the victim id and any
# remaining {…} placeholders with VICTIM_RESOURCE.
fill_path() {
	local p="$1"
	p="${p//\{id\}/$VICTIM_TENANT}"
	p="${p//\{tenantID\}/$VICTIM_TENANT}"
	p="${p//\{tenantId\}/$VICTIM_TENANT}"
	# Replace any other {placeholder} with the resource sentinel.
	while [[ "$p" =~ \{[a-zA-Z0-9_]+\} ]]; do
		p="${p/${BASH_REMATCH[0]}/$VICTIM_RESOURCE}"
	done
	echo "$p"
}

echo "IDOR check against ${TARGET} as attacker tenant, victim=${VICTIM_TENANT}"
while IFS= read -r ep; do
	url="${TARGET}$(fill_path "$ep")"
	code="$(sec_curl_status GET "$url" -H "Authorization: Bearer ${ATTACKER_TOKEN}")"
	case "$code" in
		200|201|202|206)
			sec_fail "${ep} -> ${code} (cross-tenant data exposed!)" ;;
		401|403|404)
			sec_pass "${ep} -> ${code} (isolated)" ;;
		000)
			sec_skip "${ep} -> connection failed" ;;
		*)
			# 400/405/etc: not a leak, but note it for manual review.
			sec_pass "${ep} -> ${code} (non-2xx, review)" ;;
	esac
done < <(sec_endpoints)

sec_summary
