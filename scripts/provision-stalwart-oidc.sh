#!/usr/bin/env sh
# KMail — provision Stalwart's OIDC bearer directory (findings #1/#2).
#
# The production BFF→Stalwart trust model is OIDC bearer: the BFF
# (`cmd/kmail-api`, package `internal/stalwartauth`) mints a
# short-lived, `stalwart`-audience RS256 JWT per principal and serves
# the discovery + JWKS documents Stalwart fetches to validate it. For
# Stalwart to accept those tokens it needs an OIDC *directory* whose
# `issuerUrl` points back at the BFF, made the default authentication
# directory.
#
# Unlike `scripts/stalwart-init.sh` (which runs before the BFF
# exists), this script MUST run AFTER `kmail-api` is up and serving
# its discovery/JWKS endpoints, because Stalwart fetches the JWKS at
# directory-open time and silently keeps the previous config if the
# fetch fails. It is therefore a separate, post-BFF step in the dev
# stack and CI (see `.github/workflows/ci.yml` and
# `scripts/test-e2e.sh`).
#
# v0.16.0 has NO file-based `[auth.oidc]` TOML — directories live in
# the admin registry and are written via the custom `x:Directory/set`
# + `x:Authentication/set` JMAP methods, exactly like the storage
# backends in `stalwart-init.sh`. The default-directory switch is
# latched at boot on v0.16.0, so the registry write is necessary but
# NOT sufficient — Stalwart must be restarted (with the BFF reachable
# so it can fetch the JWKS at boot) before it validates bearer tokens.
# This script performs that restart via STALWART_RESTART_CMD (see the
# activation block below); when it is unset it prints the manual step.
#
# Idempotent: if Stalwart's default directory is already the OIDC
# directory for this issuer, the script is a no-op.
#
# Inputs (env):
#   STALWART_ADMIN_URL         default: http://localhost:8080
#   STALWART_ADMIN_PASSWORD    required (recovery-admin password)
#   STALWART_ADMIN_ACCOUNT_ID  default: d333333
#   KMAIL_STALWART_OIDC_ISSUER required; MUST equal the BFF's
#                              configured issuer AND be reachable from
#                              the Stalwart container, e.g.
#                              http://host.docker.internal:8088/oidc/stalwart
#   KMAIL_STALWART_OIDC_AUDIENCE default: stalwart
#   OIDC_CLAIM_USERNAME        default: email
#   OIDC_PROBE_URL             where THIS script fetches the BFF
#                              discovery doc from, if different from
#                              the issuer (the issuer host may only be
#                              resolvable inside the Stalwart
#                              container, e.g. host.docker.internal).
#                              default: ${ISSUER}/.well-known/openid-configuration

set -eu

ADMIN_URL=${STALWART_ADMIN_URL:-http://localhost:8080}
ADMIN_USER=admin
ADMIN_PASS=${STALWART_ADMIN_PASSWORD:?STALWART_ADMIN_PASSWORD is required}
ADMIN_ACCOUNT_ID=${STALWART_ADMIN_ACCOUNT_ID:-d333333}

ISSUER=${KMAIL_STALWART_OIDC_ISSUER:?KMAIL_STALWART_OIDC_ISSUER is required}
AUDIENCE=${KMAIL_STALWART_OIDC_AUDIENCE:-stalwart}
CLAIM_USERNAME=${OIDC_CLAIM_USERNAME:-email}
DESCRIPTION=${OIDC_DIRECTORY_DESCRIPTION:-KMail BFF OIDC bearer}

log() { printf '[stalwart-oidc] %s\n' "$*"; }

# jmap_call METHOD ARGS — single-method JMAP request, Basic auth.
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
# 0. Sanity-check the BFF discovery document is reachable from this
#    host and self-consistent before we point Stalwart at it. The
#    discovery `issuer` field MUST equal the configured issuerUrl
#    (Stalwart rejects a mismatch), so verifying it here turns an
#    otherwise-silent hot-reload failure into a clear error.
# ------------------------------------------------------------------
DISCOVERY_URL=${OIDC_PROBE_URL:-${ISSUER}/.well-known/openid-configuration}
log "checking BFF discovery at ${DISCOVERY_URL}"
i=0
until disc=$(curl -fsS "$DISCOVERY_URL" 2>/dev/null); do
  i=$((i + 1))
  if [ "$i" -gt 30 ]; then
    log "timed out fetching BFF discovery at ${DISCOVERY_URL}; is kmail-api up with KMAIL_STALWART_OIDC_ISSUER set?"
    exit 1
  fi
  sleep 2
done
if ! printf '%s' "$disc" | grep -qF "\"issuer\":\"${ISSUER}\""; then
  log "discovery issuer does not equal ${ISSUER}; Stalwart would reject it. Got: ${disc}"
  exit 1
fi
log "BFF discovery reachable and issuer matches"

# ------------------------------------------------------------------
# 1. Idempotency: if the default directory is already our OIDC
#    directory for this issuer, do nothing.
# ------------------------------------------------------------------
auth_get=$(jmap_call "x:Authentication/get" "$(printf '{"accountId":"%s","ids":["singleton"]}' "$ADMIN_ACCOUNT_ID")")
CUR_DIR_ID=$(printf '%s' "$auth_get" | sed -n 's/.*"directoryId":"\([^"]*\)".*/\1/p')
if [ -n "$CUR_DIR_ID" ]; then
  dir_get=$(jmap_call "x:Directory/get" "$(printf '{"accountId":"%s","ids":["%s"]}' "$ADMIN_ACCOUNT_ID" "$CUR_DIR_ID")")
  if printf '%s' "$dir_get" | grep -q '"@type":"Oidc"' \
     && printf '%s' "$dir_get" | grep -qF "\"issuerUrl\":\"${ISSUER}\""; then
    log "default directory already OIDC for ${ISSUER} (id=${CUR_DIR_ID}); nothing to do"
    exit 0
  fi
fi

# ------------------------------------------------------------------
# 2. Create the OIDC directory. `requireScopes` is omitted on
#    purpose — Stalwart rejects a bare `[]` as an invalid patch and
#    the default (`["openid","email"]`) already matches the BFF's
#    minted `scope` claim. `claimUsername=email` selects the
#    principal from the (BFF-derived, never user-controlled) email
#    claim.
# ------------------------------------------------------------------
log "creating OIDC directory (issuerUrl=${ISSUER}, audience=${AUDIENCE}, claimUsername=${CLAIM_USERNAME})"
DIR_RECORD=$(printf '{"@type":"Oidc","description":"%s","issuerUrl":"%s","requireAudience":"%s","claimUsername":"%s"}' \
  "$DESCRIPTION" "$ISSUER" "$AUDIENCE" "$CLAIM_USERNAME")
DIR_SET_ARGS=$(printf '{"accountId":"%s","create":{"d":%s}}' "$ADMIN_ACCOUNT_ID" "$DIR_RECORD")
dir_resp=$(jmap_call "x:Directory/set" "$DIR_SET_ARGS")
DIR_ID=$(printf '%s' "$dir_resp" | sed -n 's/.*"created":{"d":{[^}]*"id":"\([^"]*\)".*/\1/p')
if [ -z "$DIR_ID" ]; then
  # Fall back to the simpler shape in case `id` is the first key.
  DIR_ID=$(printf '%s' "$dir_resp" | sed -n 's/.*"created":{"d":{"id":"\([^"]*\)".*/\1/p')
fi
if [ -z "$DIR_ID" ]; then
  log "failed to create OIDC directory: ${dir_resp}"
  exit 1
fi
log "OIDC directory created (id=${DIR_ID})"

# ------------------------------------------------------------------
# 3. Make it the default authentication directory. `Authentication`
#    is a singleton (fixed id "singleton"); pointing `directoryId` at
#    the OIDC directory makes Stalwart validate inbound bearer tokens
#    against the BFF's JWKS. The recovery admin (admin:<pass>) is
#    still honoured because Stalwart checks it BEFORE the directory,
#    so `stalwart-init.sh` provisioning keeps working.
# ------------------------------------------------------------------
AUTH_SET_ARGS=$(printf '{"accountId":"%s","update":{"singleton":{"directoryId":"%s"}}}' "$ADMIN_ACCOUNT_ID" "$DIR_ID")
auth_resp=$(jmap_call "x:Authentication/set" "$AUTH_SET_ARGS")
if ! printf '%s' "$auth_resp" | grep -q '"updated":{"singleton":'; then
  log "failed to set default directory: ${auth_resp}"
  exit 1
fi
log "default authentication directory set to OIDC (id=${DIR_ID})"

# ------------------------------------------------------------------
# 4. Activate the change.
#
#    Stalwart hot-reloads most registry writes, but switching the
#    *default authentication directory* is latched at boot — verified
#    against stalwartlabs/stalwart:v0.16.0: after this write the old
#    directory keeps serving auth until the server restarts. So the
#    registry write above is necessary but not sufficient; Stalwart
#    must be reloaded/restarted (with the BFF reachable so it can
#    fetch the JWKS at boot) before it will validate bearer tokens.
#
#    This script only owns the registry write so it stays usable
#    against a managed prod Stalwart over HTTP. If STALWART_RESTART_CMD
#    is set (dev/CI), run it here and wait for the admin API to come
#    back; otherwise print the manual step.
# ------------------------------------------------------------------
if [ -n "${STALWART_RESTART_CMD:-}" ]; then
  log "activating: ${STALWART_RESTART_CMD}"
  # shellcheck disable=SC2086
  sh -c "$STALWART_RESTART_CMD"
  log "waiting for Stalwart admin API to come back"
  i=0
  # Stderr is silenced: curl legitimately fails ("connection reset")
  # during the restart window, and the loop simply retries.
  until jmap_call "x:Authentication/get" "$(printf '{"accountId":"%s","ids":["singleton"]}' "$ADMIN_ACCOUNT_ID")" 2>/dev/null \
        | grep -q '"directoryId"'; do
    i=$((i + 1))
    if [ "$i" -gt 60 ]; then
      log "Stalwart admin API did not return within 60s after restart"
      exit 1
    fi
    sleep 1
  done
  log "OIDC bearer directory provisioned and activated; Stalwart validates BFF-minted tokens"
else
  log "OIDC bearer directory provisioned. RESTART/RELOAD Stalwart (with the BFF"
  log "reachable) to activate the default-directory change, e.g.:"
  log "  docker compose restart stalwart"
  log "or set STALWART_RESTART_CMD to have this script do it."
fi
