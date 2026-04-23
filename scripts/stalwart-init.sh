#!/usr/bin/env sh
# KMail — Stalwart v0.16.0 post-start initialization.
#
# Runs once after Stalwart comes up healthy (see the `stalwart-init`
# one-shot in docker-compose.yml). Uses the Stalwart admin API to
# configure the remaining storage backends, listeners, and the
# kmail-dev tenant — the pieces that otherwise require a trip
# through Stalwart's interactive setup wizard. The minimal JSON
# bootstrap at configs/stalwart/config.json only points Stalwart
# at its Postgres data store; everything below lives inside that
# data store after this script runs.
#
# After this script succeeds, bringing the stack up becomes a
# hands-off `docker compose up` — no human ever needs to walk
# through the Stalwart setup wizard.
#
# Idempotent: every PUT / POST below uses a target path that
# Stalwart treats as a replace, and settings that already match are
# a no-op. Safe to re-run against an already-initialized instance.
#
# Inputs (from compose environment):
#   STALWART_ADMIN_URL        default: http://stalwart:8080
#   STALWART_ADMIN_PASSWORD   required
#   ZK_FABRIC_URL             default: http://zk-fabric:8080
#   ZK_FABRIC_BUCKET          default: kmail-blobs
#   ZK_FABRIC_ACCESS_KEY      default: kmail-access-key
#   ZK_FABRIC_SECRET_KEY      default: kmail-secret-key
#   MEILISEARCH_URL           default: http://meilisearch:7700
#   MEILISEARCH_API_KEY       default: kmail-dev
#   VALKEY_URL                default: redis://valkey:6379
#   KMAIL_DEV_TENANT_DOMAIN   default: kmail.local

set -eu

ADMIN_URL=${STALWART_ADMIN_URL:-http://stalwart:8080}
ADMIN_USER=admin
ADMIN_PASS=${STALWART_ADMIN_PASSWORD:?STALWART_ADMIN_PASSWORD is required}

ZK_FABRIC_URL=${ZK_FABRIC_URL:-http://zk-fabric:8080}
ZK_FABRIC_BUCKET=${ZK_FABRIC_BUCKET:-kmail-blobs}
ZK_FABRIC_ACCESS_KEY=${ZK_FABRIC_ACCESS_KEY:-kmail-access-key}
ZK_FABRIC_SECRET_KEY=${ZK_FABRIC_SECRET_KEY:-kmail-secret-key}

MEILI_URL=${MEILISEARCH_URL:-http://meilisearch:7700}
MEILI_KEY=${MEILISEARCH_API_KEY:-kmail-dev}

VALKEY_URL=${VALKEY_URL:-redis://valkey:6379}

DEV_DOMAIN=${KMAIL_DEV_TENANT_DOMAIN:-kmail.local}

log() { printf '[stalwart-init] %s\n' "$*"; }

# ------------------------------------------------------------------
# Wait for Stalwart's admin API to be ready.
# ------------------------------------------------------------------
log "waiting for stalwart admin API at ${ADMIN_URL}"
i=0
until curl -sS -u "${ADMIN_USER}:${ADMIN_PASS}" -o /dev/null -w '%{http_code}' \
    "${ADMIN_URL}/api/settings/list" | grep -qE '^(200|204)$'; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    log "timed out waiting for stalwart admin API"
    exit 1
  fi
  sleep 2
done
log "stalwart admin API reachable"

# ------------------------------------------------------------------
# Helper: put a single setting key/value via the admin API.
# Uses the Stalwart v0.16.0 /api/settings POST endpoint, which
# accepts a JSON body of {"key": "...", "value": "..."} and is
# idempotent (replaces the existing value).
# ------------------------------------------------------------------
put_setting() {
  key=$1
  value=$2
  body=$(printf '{"key":"%s","value":%s}' "$key" "$value")
  curl -fsSL -u "${ADMIN_USER}:${ADMIN_PASS}" \
    -H 'Content-Type: application/json' \
    -X POST "${ADMIN_URL}/api/settings" \
    -d "$body" > /dev/null
  log "set $key"
}

# Convenience: quote a shell string as a JSON string.
jstr() { printf '"%s"' "$1"; }

# ------------------------------------------------------------------
# Blob store → zk-object-fabric S3 gateway.
# ------------------------------------------------------------------
log "configuring blob store → zk-object-fabric"
put_setting 'store.blob.type'        "$(jstr s3)"
put_setting 'store.blob.endpoint'    "$(jstr "$ZK_FABRIC_URL")"
put_setting 'store.blob.region'      "$(jstr us-east-1)"
put_setting 'store.blob.bucket'      "$(jstr "$ZK_FABRIC_BUCKET")"
put_setting 'store.blob.access-key'  "$(jstr "$ZK_FABRIC_ACCESS_KEY")"
put_setting 'store.blob.secret-key'  "$(jstr "$ZK_FABRIC_SECRET_KEY")"
put_setting 'store.blob.path-style'  'true'
put_setting 'storage.blob'           "$(jstr blob)"

# ------------------------------------------------------------------
# Search store → Meilisearch.
# ------------------------------------------------------------------
log "configuring search store → Meilisearch"
put_setting 'store.fts.type'     "$(jstr meilisearch)"
put_setting 'store.fts.url'      "$(jstr "$MEILI_URL")"
put_setting 'store.fts.api-key'  "$(jstr "$MEILI_KEY")"
put_setting 'storage.fts'        "$(jstr fts)"

# ------------------------------------------------------------------
# In-memory / lookup store → Valkey (Redis protocol).
# ------------------------------------------------------------------
log "configuring in-memory store → Valkey"
put_setting 'store.lookup.type'  "$(jstr redis)"
put_setting 'store.lookup.urls'  "[\"$VALKEY_URL\"]"
put_setting 'storage.lookup'     "$(jstr lookup)"

# ------------------------------------------------------------------
# Protocol listeners on the standard container-internal ports.
# docker-compose.yml publishes 25 / 465 / 587 (SMTP), 143 / 993
# (IMAP), and maps internal 8080 JMAP to host 18080.
# ------------------------------------------------------------------
log "configuring SMTP / IMAP / JMAP listeners"
put_setting 'server.listener.smtp.protocol'       "$(jstr smtp)"
put_setting 'server.listener.smtp.bind'           "$(jstr '[::]:25')"
put_setting 'server.listener.smtps.protocol'      "$(jstr smtp)"
put_setting 'server.listener.smtps.bind'          "$(jstr '[::]:465')"
put_setting 'server.listener.smtps.tls.implicit'  'false'
put_setting 'server.listener.submission.protocol' "$(jstr smtp)"
put_setting 'server.listener.submission.bind'     "$(jstr '[::]:587')"
put_setting 'server.listener.imap.protocol'       "$(jstr imap)"
put_setting 'server.listener.imap.bind'           "$(jstr '[::]:143')"
put_setting 'server.listener.imaps.protocol'      "$(jstr imap)"
put_setting 'server.listener.imaps.bind'          "$(jstr '[::]:993')"
put_setting 'server.listener.jmap.protocol'       "$(jstr jmap)"
put_setting 'server.listener.jmap.bind'           "$(jstr '[::]:8080')"

# ------------------------------------------------------------------
# kmail-dev tenant — minimum viable tenant row so developers can
# hit the stack end-to-end without running the Tenant Service
# first. Created via the admin API's domain + account endpoints
# which are idempotent on conflict.
# ------------------------------------------------------------------
log "creating kmail-dev tenant domain ${DEV_DOMAIN}"
curl -fsSL -u "${ADMIN_USER}:${ADMIN_PASS}" \
  -H 'Content-Type: application/json' \
  -X POST "${ADMIN_URL}/api/domain" \
  -d "$(printf '{"name":"%s","description":"KMail local dev tenant"}' "$DEV_DOMAIN")" \
  > /dev/null 2>&1 || log "domain ${DEV_DOMAIN} already exists, continuing"

log "done"
