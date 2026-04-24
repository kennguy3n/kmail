#!/usr/bin/env sh
# KMail — Stalwart v0.16.0 post-start initialization.
#
# Runs once after Stalwart comes up healthy (see the `stalwart-init`
# one-shot in docker-compose.yml). Drives Stalwart's admin registry
# via JMAP so the storage backends and the kmail-dev tenant land in
# the data store without a human walking through the first-boot
# setup wizard.
#
# Why JMAP and not `/api/settings`: Stalwart v0.16.0 dropped the
# legacy REST surface the earlier revision of this script POSTed
# against. The entire admin configuration now lives behind JMAP
# method calls of the shape `x:<ObjectType>/{get,set,query}` against
# the regular `/jmap` endpoint. Basic auth with the recovery admin
# works on the same listener, so the init script doesn't need to
# run the OAuth/OIDC dance the admin SPA uses. The per-object JSON
# shapes below match Stalwart's live `/api/schema` response at
# v0.16.0 (verified against `crates/store/src/backend/s3/mod.rs`
# for the S3Store.region.Custom variant).
#
# After this script succeeds, bringing the stack up becomes a
# hands-off `docker compose up` — no human ever needs to walk
# through the setup wizard.
#
# Idempotent: every call below either checks for an existing record
# first (singletons: BlobStore, InMemoryStore, SearchStore) or
# queries for the target before creating (Domain). Safe to re-run
# against an already-initialized instance.
#
# Inputs (from compose environment):
#   STALWART_ADMIN_URL         default: http://stalwart:8080
#   STALWART_ADMIN_PASSWORD    required
#   STALWART_ADMIN_ACCOUNT_ID  default: d333333 (recovery admin in v0.16.0)
#   ZK_FABRIC_URL              default: http://zk-fabric:8080
#   ZK_FABRIC_BUCKET           default: kmail-blobs
#   ZK_FABRIC_ACCESS_KEY       default: kmail-access-key
#   ZK_FABRIC_SECRET_KEY       default: kmail-secret-key
#   MEILISEARCH_URL            default: http://meilisearch:7700
#   MEILISEARCH_API_KEY        default: kmail-dev
#   VALKEY_URL                 default: redis://valkey:6379
#   KMAIL_DEV_TENANT_DOMAIN    default: kmail.dev

set -eu

ADMIN_URL=${STALWART_ADMIN_URL:-http://stalwart:8080}
ADMIN_USER=admin
ADMIN_PASS=${STALWART_ADMIN_PASSWORD:?STALWART_ADMIN_PASSWORD is required}
# Stalwart assigns this deterministic ID to the recovery admin
# bootstrapped from `STALWART_RECOVERY_ADMIN=admin:<pass>` on a
# fresh datastore. Every JMAP admin call is tagged with this
# accountId so the registry writes land on the right principal.
ADMIN_ACCOUNT_ID=${STALWART_ADMIN_ACCOUNT_ID:-d333333}

ZK_FABRIC_URL=${ZK_FABRIC_URL:-http://zk-fabric:8080}
ZK_FABRIC_BUCKET=${ZK_FABRIC_BUCKET:-kmail-blobs}
ZK_FABRIC_ACCESS_KEY=${ZK_FABRIC_ACCESS_KEY:-kmail-access-key}
ZK_FABRIC_SECRET_KEY=${ZK_FABRIC_SECRET_KEY:-kmail-secret-key}

MEILI_URL=${MEILISEARCH_URL:-http://meilisearch:7700}
MEILI_KEY=${MEILISEARCH_API_KEY:-kmail-dev}

VALKEY_URL=${VALKEY_URL:-redis://valkey:6379}

# Stalwart v0.16.0's domain validator rejects RFC 2606 / mDNS-style
# suffixes like `.local`, `.test`, and `localhost.localdomain`.
# `kmail.dev` is a real TLD owned by Google and works as a dev
# default without surprising the validator.
DEV_DOMAIN=${KMAIL_DEV_TENANT_DOMAIN:-kmail.dev}

log() { printf '[stalwart-init] %s\n' "$*"; }

# ------------------------------------------------------------------
# Low-level JMAP helper.
# ------------------------------------------------------------------

# jmap_call METHOD_NAME ARGS_JSON
# POSTs a single-method JMAP request with Basic auth. `args` must be
# a complete JSON object; accountId is embedded by the caller.
# Echoes the full response body on stdout. Exits non-zero on
# curl-level failures (network / HTTP 5xx). JMAP-level
# `notCreated` / `notUpdated` responses come back as HTTP 200 and
# are inspected by the caller.
jmap_call() {
  method=$1
  args=$2
  body=$(printf '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["%s",%s,"c1"]]}' "$method" "$args")
  curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" \
    -H 'Content-Type: application/json' \
    -X POST "${ADMIN_URL}/jmap" \
    -d "$body"
}

# ------------------------------------------------------------------
# Wait for Stalwart's JMAP endpoint to answer with Basic auth.
# ------------------------------------------------------------------
log "waiting for stalwart JMAP endpoint at ${ADMIN_URL}"
i=0
until curl -sS -u "${ADMIN_USER}:${ADMIN_PASS}" -o /dev/null -w '%{http_code}' \
    "${ADMIN_URL}/jmap/session" | grep -qE '^(200|204)$'; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    log "timed out waiting for stalwart JMAP endpoint"
    exit 1
  fi
  sleep 2
done
log "stalwart JMAP endpoint reachable"

# ------------------------------------------------------------------
# singleton_upsert OBJECT RECORD_JSON
# BlobStore, InMemoryStore and SearchStore are single-instance
# registry objects in v0.16.0 — the server assigns them the fixed
# id "singleton". Stalwart auto-creates each as a `Default` variant
# on first boot, so the store pointer exists before this script
# runs but points at the wrong backend. We therefore always issue
# `/set` with an `update`: that overwrites the auto-created Default
# (switching `@type` to S3 / Redis / Meilisearch), and on re-runs
# it's an idempotent no-op. A JMAP `update` response comes back as
# `{"updated":{"singleton":null}}` on success or
# `{"notUpdated":{"singleton":{"type":"..."}}}` on failure.
# ------------------------------------------------------------------
singleton_upsert() {
  object=$1
  record=$2
  set_args=$(printf '{"accountId":"%s","update":{"singleton":%s}}' "$ADMIN_ACCOUNT_ID" "$record")
  response=$(jmap_call "x:${object}/set" "$set_args")
  if printf '%s' "$response" | grep -q '"updated":{"singleton":'; then
    log "${object} configured"
    return 0
  fi
  log "failed to configure ${object}: ${response}"
  exit 1
}

# ------------------------------------------------------------------
# Blob store → zk-object-fabric S3 gateway.
# `S3StoreRegion::Custom` lets us point Stalwart's S3 client at a
# non-AWS endpoint (zk-fabric on the compose network). See
# `crates/store/src/backend/s3/mod.rs` in Stalwart v0.16.0 for the
# mapping to `rust-s3`'s `Region::Custom { region, endpoint }`.
# ------------------------------------------------------------------
log "configuring blob store → zk-object-fabric"
BLOB_RECORD=$(cat <<JSON
{
  "@type": "S3",
  "bucket": "${ZK_FABRIC_BUCKET}",
  "region": {
    "@type": "Custom",
    "customRegion": "us-east-1",
    "customEndpoint": "${ZK_FABRIC_URL}"
  },
  "accessKey": "${ZK_FABRIC_ACCESS_KEY}",
  "secretKey": { "@type": "Value", "secret": "${ZK_FABRIC_SECRET_KEY}" }
}
JSON
)
singleton_upsert BlobStore "$BLOB_RECORD"

# ------------------------------------------------------------------
# In-memory / lookup store → Valkey (Redis protocol).
# ------------------------------------------------------------------
log "configuring in-memory store → Valkey"
INMEM_RECORD=$(cat <<JSON
{
  "@type": "Redis",
  "url": "${VALKEY_URL}"
}
JSON
)
singleton_upsert InMemoryStore "$INMEM_RECORD"

# ------------------------------------------------------------------
# Search / FTS store → Meilisearch.
# Meilisearch accepts a master key as a Bearer token (see
# https://www.meilisearch.com/docs/reference/api/overview#authorization).
# ------------------------------------------------------------------
log "configuring search store → Meilisearch"
SEARCH_RECORD=$(cat <<JSON
{
  "@type": "Meilisearch",
  "url": "${MEILI_URL}",
  "httpAuth": {
    "@type": "Bearer",
    "bearerToken": { "@type": "Value", "secret": "${MEILI_KEY}" }
  }
}
JSON
)
singleton_upsert SearchStore "$SEARCH_RECORD"

# ------------------------------------------------------------------
# kmail-dev tenant domain.
# Non-singleton — the server assigns a fresh id on each create. We
# query first so re-runs are no-ops rather than 409s.
# ------------------------------------------------------------------
log "creating kmail-dev tenant domain ${DEV_DOMAIN}"
DOMAIN_QUERY_ARGS=$(printf '{"accountId":"%s","filter":{"name":"%s"}}' "$ADMIN_ACCOUNT_ID" "$DEV_DOMAIN")
existing=$(jmap_call "x:Domain/query" "$DOMAIN_QUERY_ARGS")
if printf '%s' "$existing" | grep -q '"ids":\["[^"]'; then
  log "domain ${DEV_DOMAIN} already exists, skipping"
else
  DOMAIN_SET_ARGS=$(printf '{"accountId":"%s","create":{"d":{"name":"%s"}}}' "$ADMIN_ACCOUNT_ID" "$DEV_DOMAIN")
  response=$(jmap_call "x:Domain/set" "$DOMAIN_SET_ARGS")
  if printf '%s' "$response" | grep -q '"created":{"d":'; then
    log "domain ${DEV_DOMAIN} created"
  else
    log "failed to create domain ${DEV_DOMAIN}: ${response}"
    exit 1
  fi
fi

# Network listeners (SMTP / IMAP / JMAP / HTTP) on the standard
# port bindings (25 / 465 / 587 / 143 / 993 / 8080) are created
# automatically by Stalwart on first boot and are already present
# when this script runs. Left as a comment so nobody comes looking
# for a missing `x:NetworkListener/set` call.

# ------------------------------------------------------------------
# First-boot restart of the stalwart container.
# ------------------------------------------------------------------
# Stalwart v0.16.0 resolves the concrete blob / in-memory / search
# backends from the registry at startup — subsequent /set writes
# land in Postgres but don't swap the live backend pointer the mail
# core is holding. On a fresh volume that means:
#
#   1. Stalwart boots with an empty registry → falls back to the
#      Postgres-backed Default BlobStore for blob writes.
#   2. This script writes the S3 / Redis / Meilisearch singletons
#      into the registry.
#   3. Stalwart doesn't see them until it restarts.
#
# After the restart Stalwart reads the singletons on startup and
# all blob writes flow to zk-object-fabric. Subsequent compose
# boots pick up the config immediately on their very first read —
# this restart is a one-time first-boot thing.
#
# We drive the restart via Docker's HTTP Engine API over a
# bind-mounted `/var/run/docker.sock`. The init container doesn't
# have the `docker` CLI, so we POST directly
# (`POST /containers/{id}/restart`, see
# https://docs.docker.com/engine/api/). If the socket isn't
# mounted (non-compose environments) the script logs a hint and
# exits cleanly so the operator can restart Stalwart themselves.
DOCKER_SOCK=${DOCKER_SOCK:-/var/run/docker.sock}
STALWART_CONTAINER=${STALWART_CONTAINER_NAME:-kmail-stalwart}
if [ -S "$DOCKER_SOCK" ]; then
  log "restarting ${STALWART_CONTAINER} via docker socket so the new stores take effect"
  code=$(curl -sS --unix-socket "$DOCKER_SOCK" -o /dev/null -w '%{http_code}' \
           -X POST "http://localhost/containers/${STALWART_CONTAINER}/restart?t=5")
  if [ "$code" != "204" ]; then
    log "docker restart returned HTTP ${code}; skipping wait"
  else
    log "waiting for ${STALWART_CONTAINER} JMAP endpoint to come back up"
    i=0
    until curl -sS -u "${ADMIN_USER}:${ADMIN_PASS}" -o /dev/null -w '%{http_code}' \
        "${ADMIN_URL}/jmap/session" | grep -qE '^(200|204)$'; do
      i=$((i + 1))
      if [ "$i" -gt 60 ]; then
        log "timed out waiting for stalwart to come back; continuing"
        break
      fi
      sleep 2
    done
    log "${STALWART_CONTAINER} is back up"
  fi
else
  log "docker socket not mounted at ${DOCKER_SOCK}; skipping restart"
  log "restart stalwart manually so the new blob store takes effect"
fi

log "done"
