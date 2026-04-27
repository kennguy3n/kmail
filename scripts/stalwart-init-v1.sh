#!/usr/bin/env sh
# KMail — Stalwart v1.0.0 post-start initialization (preview).
#
# Targets the expected v1.0.0 admin API shape. v1.0.0 is the
# upcoming major release expected H1 2026 (see
# `docs/STALWART_UPGRADE.md` and the version pin note in
# `README.md`); this script is the parallel of
# `scripts/stalwart-init.sh` we will switch to once v1.0.0 ships
# and the staging upgrade plan is complete.
#
# Documented assumptions about v1.0.0:
#
#   * Admin methods land under the typed namespace
#     `urn:stalwart:admin:<Type>/<verb>` instead of v0.16.0's
#     `x:<Type>/<verb>`. The wire shape (envelope, ids, args) is
#     otherwise unchanged.
#   * The JMAP session response advertises `stalwartVersion`
#     directly (alongside `capabilities`) so downstream callers
#     no longer have to parse the `Server:` header.
#   * Capability URNs migrate to the IANA registry; the legacy
#     `urn:stalwart:*` URNs stay accepted for at least one major
#     version.
#   * Recovery admin auth still works via HTTP basic on /jmap.
#
# The actual v1.0.0 release will publish a migration note;
# `docs/STALWART_UPGRADE.md` tracks any deltas vs the
# assumptions above so this script can be amended once the
# upstream changelog lands.
#
# Inputs (from compose environment):
#   STALWART_URL       — http://stalwart:8080 by default.
#   STALWART_ADMIN_USER / STALWART_ADMIN_PASS — recovery admin.
#   ZK_FABRIC_S3_URL / ZK_FABRIC_ACCESS_KEY / ZK_FABRIC_SECRET_KEY
#                      — blob store wiring.
#   MEILISEARCH_URL / MEILISEARCH_KEY — search backend wiring.
#   VALKEY_URL         — in-memory store wiring.
#   STALWART_BOOTSTRAP_DOMAIN — bootstrap domain (default kmail-dev).

set -eu

URL="${STALWART_URL:-http://stalwart:8080}"
ADMIN_USER="${STALWART_ADMIN_USER:-admin}"
ADMIN_PASS="${STALWART_ADMIN_PASS:-stalwart}"
DOMAIN="${STALWART_BOOTSTRAP_DOMAIN:-kmail-dev}"
S3_URL="${ZK_FABRIC_S3_URL:-http://zk-fabric:8080}"
S3_KEY="${ZK_FABRIC_ACCESS_KEY:-kmail-dev}"
S3_SECRET="${ZK_FABRIC_SECRET_KEY:-kmail-dev}"
MEILI_URL="${MEILISEARCH_URL:-http://meilisearch:7700}"
MEILI_KEY="${MEILISEARCH_KEY:-kmail-dev}"
VALKEY="${VALKEY_URL:-redis://valkey:6379}"

jmap() {
  curl --fail --silent --show-error \
       --user "$ADMIN_USER:$ADMIN_PASS" \
       --header 'Content-Type: application/json' \
       --data "$1" \
       "$URL/jmap"
}

# Wait for the JMAP session endpoint to come up. v1.0.0 returns
# `stalwartVersion` directly; bail if the version is not v1.x so
# this script never silently runs against a v0.16.0 instance.
echo "[stalwart-init-v1] waiting for $URL/jmap/session"
i=0
while :; do
  body="$(curl --fail --silent --user "$ADMIN_USER:$ADMIN_PASS" "$URL/jmap/session" || echo '')"
  if [ -n "$body" ]; then
    case "$body" in
      *'"stalwartVersion":"v1'* | *'"stalwartVersion":"1.'*) break ;;
      *) echo "[stalwart-init-v1] not v1 yet: $body" ;;
    esac
  fi
  i=$((i + 1))
  if [ $i -gt 60 ]; then
    echo "[stalwart-init-v1] timed out waiting for v1.x" >&2
    exit 1
  fi
  sleep 2
done

echo "[stalwart-init-v1] configuring blob store -> S3"
jmap "$(cat <<JSON
{"using":["urn:ietf:params:jmap:core","urn:stalwart:admin:core"],
 "methodCalls":[["urn:stalwart:admin:BlobStore/set",{
   "create":{"primary":{"backend":"s3","endpoint":"$S3_URL",
   "bucket":"kmail-stalwart","region":"custom","accessKey":"$S3_KEY",
   "secretKey":"$S3_SECRET","pathStyle":true}}},"c0"]]}
JSON
)" >/dev/null

echo "[stalwart-init-v1] configuring search store -> Meilisearch"
jmap "$(cat <<JSON
{"using":["urn:ietf:params:jmap:core","urn:stalwart:admin:core"],
 "methodCalls":[["urn:stalwart:admin:SearchStore/set",{
   "create":{"primary":{"backend":"meilisearch","url":"$MEILI_URL",
   "apiKey":"$MEILI_KEY"}}},"c0"]]}
JSON
)" >/dev/null

echo "[stalwart-init-v1] configuring in-memory store -> Valkey"
jmap "$(cat <<JSON
{"using":["urn:ietf:params:jmap:core","urn:stalwart:admin:core"],
 "methodCalls":[["urn:stalwart:admin:InMemoryStore/set",{
   "create":{"primary":{"backend":"redis","url":"$VALKEY"}}},"c0"]]}
JSON
)" >/dev/null

echo "[stalwart-init-v1] creating bootstrap domain $DOMAIN"
jmap "$(cat <<JSON
{"using":["urn:ietf:params:jmap:core","urn:stalwart:admin:core"],
 "methodCalls":[["urn:stalwart:admin:Domain/set",{
   "create":{"d0":{"name":"$DOMAIN"}}},"c0"]]}
JSON
)" >/dev/null

echo "[stalwart-init-v1] done"
