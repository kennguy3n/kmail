#!/usr/bin/env bash
#
# gen-endpoints.sh — regenerate tenant-endpoints.txt by scanning the
# Go route registrations for tenant-scoped paths. Run from anywhere;
# resolves the repo root relative to this script.
#
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="${REPO_ROOT}/scripts/security/tenant-endpoints.txt"

{
	echo "# Tenant-scoped API endpoints (auto-derived from route registrations)."
	echo "# Used by idor-check.sh to verify tenant A cannot reach tenant B's"
	echo "# resources. Regenerate with scripts/security/gen-endpoints.sh."
	echo "# {id}/{tenantID} is the tenant boundary; substitute the victim tenant's id."
	grep -rhoE "(HandleFunc|Handle|Get|Post|Put|Delete|Patch)\(\s*\"?(GET|POST|PUT|DELETE|PATCH)?\s*/api/v1/[a-zA-Z0-9/_.:{}-]+" \
		"${REPO_ROOT}/internal" "${REPO_ROOT}/cmd" 2>/dev/null \
		| grep -oE "/api/v1/[a-zA-Z0-9/_.:{}-]+" \
		| sort -u \
		| grep -E "/api/v1/(tenants/\{(id|tenantID)\}|admin/proxy/\{tenantId\})"
} > "$OUT"

echo "wrote $(grep -cvE '^#' "$OUT") endpoints to $OUT"
