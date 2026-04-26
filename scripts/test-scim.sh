#!/usr/bin/env bash
# SCIM 2.0 conformance harness for KMail.
#
# Provisions a test tenant + SCIM bearer token via the KMail BFF
# admin API, then exercises the SCIM 2.0 core surface (RFC 7643 /
# 7644) against `/scim/v2/...`. Designed to mirror the checks the
# SCIM 2.0 reference test runner makes
# (https://github.com/SCIM-Compliance/scim2-compliance-test-suite)
# without requiring a Maven / JVM toolchain on the box. Coverage
# is documented in `docs/SCIM_CONFORMANCE.md`.
#
# Usage:
#   scripts/test-scim.sh [BASE_URL]
#
#   BASE_URL defaults to http://localhost:8080 (matches the local
#   compose stack BFF). Override via `KMAIL_API_URL` env or first
#   positional arg.
#
# Exit code 0 = all checks pass; non-zero = at least one failure.

set -euo pipefail

BASE_URL="${1:-${KMAIL_API_URL:-http://localhost:8080}}"
ADMIN_BEARER="${KMAIL_DEV_BYPASS_TOKEN:-kmail-dev}"
TENANT_NAME="${SCIM_TEST_TENANT_NAME:-scim-test-$(date +%s)}"

PASS=0
FAIL=0
RESULTS=()

# pretty-print pass/fail line and update counters.
record() {
  local result="$1" name="$2" detail="${3:-}"
  if [[ "$result" == "PASS" ]]; then
    PASS=$((PASS + 1))
    printf '  \033[32mPASS\033[0m  %-48s %s\n' "$name" "$detail"
  else
    FAIL=$((FAIL + 1))
    printf '  \033[31mFAIL\033[0m  %-48s %s\n' "$name" "$detail"
  fi
  RESULTS+=("$result|$name|$detail")
}

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "scim-test: missing required tool: $1" >&2
    exit 2
  }
}

require curl
require jq

echo "SCIM 2.0 conformance harness"
echo "  base_url=$BASE_URL"
echo "  tenant=$TENANT_NAME"
echo

# ---------------------------------------------------------------
# 1. Provision a test tenant via the admin API.
# ---------------------------------------------------------------
admin_headers=(-H "Authorization: Bearer ${ADMIN_BEARER}" -H "Content-Type: application/json")

tenant_resp="$(curl -fsS -X POST "${BASE_URL}/api/v1/tenants" "${admin_headers[@]}" \
  -d "{\"name\":\"${TENANT_NAME}\",\"slug\":\"${TENANT_NAME}\",\"plan\":\"core\"}" || true)"
TENANT_ID="$(echo "$tenant_resp" | jq -r '.id // empty' 2>/dev/null || true)"
if [[ -z "$TENANT_ID" ]]; then
  echo "scim-test: failed to provision tenant via POST /api/v1/tenants" >&2
  echo "scim-test: response: $tenant_resp" >&2
  exit 2
fi
echo "  tenant_id=$TENANT_ID"

# Generate a SCIM bearer token for the tenant.
token_resp="$(curl -fsS -X POST \
  "${BASE_URL}/api/v1/tenants/${TENANT_ID}/scim/tokens" \
  "${admin_headers[@]}" -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID}" \
  -d '{"description":"scim-conformance-harness"}')"
SCIM_TOKEN="$(echo "$token_resp" | jq -r '.token // empty')"
if [[ -z "$SCIM_TOKEN" ]]; then
  echo "scim-test: failed to mint SCIM token" >&2
  echo "scim-test: response: $token_resp" >&2
  exit 2
fi
scim_headers=(-H "Authorization: Bearer ${SCIM_TOKEN}" -H "Content-Type: application/scim+json")

echo
echo "  scim_token=<redacted>"
echo

# ---------------------------------------------------------------
# 2. Discovery (RFC 7644 §4) — public, no auth.
# ---------------------------------------------------------------
echo "Discovery"

spc="$(curl -fsS "${BASE_URL}/scim/v2/ServiceProviderConfig" || echo '{}')"
if echo "$spc" | jq -e '.schemas[] | select(. == "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig")' >/dev/null 2>&1; then
  record PASS "GET /ServiceProviderConfig" "schema present"
else
  record FAIL "GET /ServiceProviderConfig" "missing schema URI"
fi

rt="$(curl -fsS "${BASE_URL}/scim/v2/ResourceTypes" || echo '{}')"
rt_count="$(echo "$rt" | jq -r '.totalResults // 0')"
if [[ "$rt_count" == "2" ]]; then
  record PASS "GET /ResourceTypes" "User + Group exposed"
else
  record FAIL "GET /ResourceTypes" "expected 2, got $rt_count"
fi

sch="$(curl -fsS "${BASE_URL}/scim/v2/Schemas" || echo '{}')"
sch_count="$(echo "$sch" | jq -r '.totalResults // 0')"
if [[ "$sch_count" -ge 2 ]]; then
  record PASS "GET /Schemas" "$sch_count schemas exposed"
else
  record FAIL "GET /Schemas" "expected >=2, got $sch_count"
fi

# ---------------------------------------------------------------
# 3. Authentication.
# ---------------------------------------------------------------
echo
echo "Authentication"
unauth="$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/scim/v2/Users")"
if [[ "$unauth" == "401" ]]; then
  record PASS "missing-bearer rejected" "HTTP 401"
else
  record FAIL "missing-bearer rejected" "expected 401, got $unauth"
fi

bad="$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer not-a-real-token" \
  "${BASE_URL}/scim/v2/Users")"
if [[ "$bad" == "401" ]]; then
  record PASS "invalid-bearer rejected" "HTTP 401"
else
  record FAIL "invalid-bearer rejected" "expected 401, got $bad"
fi

# ---------------------------------------------------------------
# 4. Users CRUD.
# ---------------------------------------------------------------
echo
echo "Users"
USER_NAME="conformance-$(date +%s)@${TENANT_NAME}.local"
create_resp="$(curl -fsS -X POST "${BASE_URL}/scim/v2/Users" "${scim_headers[@]}" -d @- <<EOF
{
  "schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
  "userName": "${USER_NAME}",
  "displayName": "Conformance User",
  "active": true,
  "name": {"givenName": "Conf", "familyName": "User"},
  "emails": [{"value": "${USER_NAME}", "primary": true, "type": "work"}]
}
EOF
)"
USER_ID="$(echo "$create_resp" | jq -r '.id // empty')"
if [[ -n "$USER_ID" ]]; then
  record PASS "POST /Users" "id=$USER_ID"
else
  record FAIL "POST /Users" "no id in response"
fi

if echo "$create_resp" | jq -e '.meta.location | test("/scim/v2/Users/")' >/dev/null 2>&1; then
  record PASS "POST /Users sets meta.location" "matches /scim/v2/Users/"
else
  record FAIL "POST /Users sets meta.location" "missing or malformed"
fi

if [[ -n "$USER_ID" ]]; then
  get_resp="$(curl -fsS "${BASE_URL}/scim/v2/Users/${USER_ID}" "${scim_headers[@]}")"
  if [[ "$(echo "$get_resp" | jq -r '.userName')" == "$USER_NAME" ]]; then
    record PASS "GET /Users/{id}" "userName roundtrips"
  else
    record FAIL "GET /Users/{id}" "userName mismatch"
  fi

  list_resp="$(curl -fsS "${BASE_URL}/scim/v2/Users?startIndex=1&count=10" "${scim_headers[@]}")"
  if echo "$list_resp" | jq -e --arg id "$USER_ID" '.Resources[] | select(.id == $id)' >/dev/null 2>&1; then
    record PASS "GET /Users (list)" "created user appears"
  else
    record FAIL "GET /Users (list)" "created user missing"
  fi

  patch_resp="$(curl -fsS -X PATCH "${BASE_URL}/scim/v2/Users/${USER_ID}" "${scim_headers[@]}" -d '{
    "schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
    "Operations": [{"op":"replace","path":"active","value":false}]
  }')"
  if [[ "$(echo "$patch_resp" | jq -r '.active')" == "false" ]]; then
    record PASS "PATCH /Users/{id} active=false" "deactivated"
  else
    record FAIL "PATCH /Users/{id} active=false" "still active"
  fi

  del="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
    "${BASE_URL}/scim/v2/Users/${USER_ID}" "${scim_headers[@]}")"
  if [[ "$del" == "204" || "$del" == "200" ]]; then
    record PASS "DELETE /Users/{id}" "HTTP $del"
  else
    record FAIL "DELETE /Users/{id}" "expected 204/200, got $del"
  fi
fi

# ---------------------------------------------------------------
# 5. Groups CRUD.
# ---------------------------------------------------------------
echo
echo "Groups"
GROUP_NAME="conformance-grp-$(date +%s)"
gcreate="$(curl -fsS -X POST "${BASE_URL}/scim/v2/Groups" "${scim_headers[@]}" -d @- <<EOF
{
  "schemas": ["urn:ietf:params:scim:schemas:core:2.0:Group"],
  "displayName": "${GROUP_NAME}"
}
EOF
)"
GROUP_ID="$(echo "$gcreate" | jq -r '.id // empty')"
if [[ -n "$GROUP_ID" ]]; then
  record PASS "POST /Groups" "id=$GROUP_ID"
else
  record FAIL "POST /Groups" "no id in response"
fi

if [[ -n "$GROUP_ID" ]]; then
  glist="$(curl -fsS "${BASE_URL}/scim/v2/Groups" "${scim_headers[@]}")"
  if echo "$glist" | jq -e --arg id "$GROUP_ID" '.Resources[] | select(.id == $id)' >/dev/null 2>&1; then
    record PASS "GET /Groups (list)" "created group appears"
  else
    record FAIL "GET /Groups (list)" "created group missing"
  fi

  del="$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
    "${BASE_URL}/scim/v2/Groups/${GROUP_ID}" "${scim_headers[@]}")"
  if [[ "$del" == "204" || "$del" == "200" ]]; then
    record PASS "DELETE /Groups/{id}" "HTTP $del"
  else
    record FAIL "DELETE /Groups/{id}" "expected 204/200, got $del"
  fi
fi

# ---------------------------------------------------------------
# 6. Error envelope shape.
# ---------------------------------------------------------------
echo
echo "Error envelope"
not_found="$(curl -s -o /tmp/scim_err.json -w '%{http_code}' \
  "${BASE_URL}/scim/v2/Users/00000000-0000-0000-0000-000000000000" "${scim_headers[@]}")"
if [[ "$not_found" == "404" ]] && jq -e '.schemas[] | select(. == "urn:ietf:params:scim:api:messages:2.0:Error")' /tmp/scim_err.json >/dev/null 2>&1; then
  record PASS "404 returns SCIM Error envelope" "RFC 7644 §3.12"
else
  record FAIL "404 returns SCIM Error envelope" "got HTTP $not_found"
fi

# ---------------------------------------------------------------
# Summary.
# ---------------------------------------------------------------
echo
total=$((PASS + FAIL))
echo "Summary: $PASS / $total passed, $FAIL failed."
if [[ "$FAIL" -ne 0 ]]; then
  exit 1
fi
exit 0
