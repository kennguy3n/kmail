#!/usr/bin/env sh
# KMail — end-to-end smoke test.
#
# Exercises the public surface of the BFF against the local
# docker-compose stack so a CI run can detect regressions in the
# top 10 user-visible workflows in under a minute. Each step is
# wrapped in `step` so the output is easy to scan, and individual
# step failures are surfaced via the exit code without aborting
# the whole run (so the report is complete even when one stage
# fails).
#
# Inputs (all have sensible compose-stack defaults):
#   KMAIL_API_URL       — BFF base URL (default http://localhost:8080)
#   KMAIL_DEV_BEARER    — dev-bypass bearer token (default kmail-dev)
#   KMAIL_E2E_TENANT    — pre-existing tenant id used for read paths
#                         (auto-discovered if unset)
#
# Requires: curl, jq.

set -u

API="${KMAIL_API_URL:-http://localhost:8080}"
TOK="${KMAIL_DEV_BEARER:-kmail-dev}"
FAIL=0

step() {
  printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"
}

ok() {
  printf '  ok\n'
}

fail() {
  printf '  FAIL: %s\n' "$*" 1>&2
  FAIL=$((FAIL + 1))
}

require() {
  command -v "$1" >/dev/null 2>&1 || {
    printf 'kmail-e2e: %s required\n' "$1" 1>&2
    exit 2
  }
}
require curl
require jq

curl_json() {
  curl --fail --silent --show-error -H "Authorization: Bearer ${TOK}" \
    -H 'Accept: application/json' "$@"
}

# ---------------------------------------------------------------
# 1. Health / readiness
# ---------------------------------------------------------------
step '1. /healthz and /readyz'
if curl --fail --silent "${API}/healthz" >/dev/null; then ok
else fail "healthz unreachable"; fi
if curl --fail --silent "${API}/readyz" >/dev/null; then ok
else fail "readyz unreachable"; fi

# ---------------------------------------------------------------
# 2. Tenant CRUD
# ---------------------------------------------------------------
step '2. Tenant list + read'
TENANTS_JSON=$(curl_json "${API}/api/v1/tenants" || echo '[]')
TENANT_ID="${KMAIL_E2E_TENANT:-$(printf '%s' "${TENANTS_JSON}" | jq -r '.[0].id // empty')}"
if [ -n "${TENANT_ID}" ]; then ok
else fail "no tenants found; create one first"; fi

# ---------------------------------------------------------------
# 3. Domain verification surface
# ---------------------------------------------------------------
step '3. Domain list for tenant'
if [ -n "${TENANT_ID}" ]; then
  if curl_json "${API}/api/v1/tenants/${TENANT_ID}/domains" \
       -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID}" >/dev/null; then ok
  else fail "domains endpoint failed"; fi
fi

# ---------------------------------------------------------------
# 4. JMAP session
# ---------------------------------------------------------------
step '4. JMAP /jmap/session'
if curl_json "${API}/jmap/session" \
     -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID:-}" >/dev/null; then ok
else fail "JMAP session fetch failed"; fi

# ---------------------------------------------------------------
# 5. Email send + receive (best effort — requires populated mailbox)
# ---------------------------------------------------------------
step '5. JMAP Email/query (round-trip probe)'
JMAP_REQ='{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/query",{"accountId":"a"},"0"]]}'
if curl --fail --silent -H "Authorization: Bearer ${TOK}" \
     -H 'Content-Type: application/json' \
     -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID:-}" \
     -d "${JMAP_REQ}" "${API}/jmap" >/dev/null; then ok
else fail "JMAP Email/query failed"; fi

# ---------------------------------------------------------------
# 6. Calendar event CRUD surface
# ---------------------------------------------------------------
step '6. Calendar bridge list events'
if [ -n "${TENANT_ID}" ]; then
  if curl_json "${API}/api/v1/tenants/${TENANT_ID}/calendar/events" \
       -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID}" >/dev/null; then ok
  else fail "calendar events endpoint failed"; fi
fi

# ---------------------------------------------------------------
# 7. Search query
# ---------------------------------------------------------------
step '7. JMAP Email/query with text filter'
SEARCH_REQ='{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/query",{"accountId":"a","filter":{"text":"hello"}},"0"]]}'
if curl --fail --silent -H "Authorization: Bearer ${TOK}" \
     -H 'Content-Type: application/json' \
     -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID:-}" \
     -d "${SEARCH_REQ}" "${API}/jmap" >/dev/null; then ok
else fail "search query failed"; fi

# ---------------------------------------------------------------
# 8. Billing summary
# ---------------------------------------------------------------
step '8. Billing summary'
if [ -n "${TENANT_ID}" ]; then
  if curl_json "${API}/api/v1/tenants/${TENANT_ID}/billing/summary" \
       -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID}" >/dev/null; then ok
  else fail "billing summary failed"; fi
fi

# ---------------------------------------------------------------
# 9. Audit log
# ---------------------------------------------------------------
step '9. Audit log query'
if [ -n "${TENANT_ID}" ]; then
  if curl_json "${API}/api/v1/tenants/${TENANT_ID}/audit?limit=5" \
       -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID}" >/dev/null; then ok
  else fail "audit log failed"; fi
fi

# ---------------------------------------------------------------
# 10. Confidential Send link create + fetch
# ---------------------------------------------------------------
step '10. Confidential Send create + portal fetch'
if [ -n "${TENANT_ID}" ]; then
  CREATE_BODY='{"sender_id":"e2e@example.com","encrypted_blob_ref":"e2e-ref","expires_in_seconds":3600,"max_views":1}'
  CREATE_RES=$(curl --fail --silent -X POST \
    -H "Authorization: Bearer ${TOK}" \
    -H "X-KMail-Dev-Tenant-Id: ${TENANT_ID}" \
    -H 'Content-Type: application/json' \
    -d "${CREATE_BODY}" \
    "${API}/api/v1/tenants/${TENANT_ID}/confidential-send" || echo '{}')
  TOKEN=$(printf '%s' "${CREATE_RES}" | jq -r '.link_token // empty')
  if [ -n "${TOKEN}" ]; then
    ok
    if curl --fail --silent "${API}/api/v1/secure/${TOKEN}" >/dev/null; then ok
    else fail "secure portal fetch failed"; fi
  else
    fail "could not create secure link (missing link_token)"
  fi
fi

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
printf '\n'
if [ "${FAIL}" -eq 0 ]; then
  printf 'kmail-e2e: all 10 stages passed\n'
  exit 0
fi
printf 'kmail-e2e: %d stage(s) failed\n' "${FAIL}" 1>&2
exit 1
