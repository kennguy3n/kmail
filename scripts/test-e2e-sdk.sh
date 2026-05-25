#!/usr/bin/env sh
# KMail — SDK end-to-end smoke test.
#
# Exercises the public surface of `kmail_core::KMailClient` (via
# the `kmail-cli` release binary) against the local docker compose
# stack. Sister script to `scripts/test-e2e.sh`, which probes the
# BFF over plain `curl`. This one closes a different gap: it
# catches wire-format regressions on the BFF↔SDK contract that the
# in-process wiremock tests in `sdk/kmail-core/src/client.rs` miss
# because they replay recorded responses, not whatever Stalwart
# actually emits on the live stack.
#
# Inputs (all have sensible compose-stack defaults):
#   KMAIL_API_URL       — BFF base URL (default http://localhost:8088)
#   KMAIL_DEV_BEARER    — dev-bypass bearer token (default kmail-dev)
#   KMAIL_E2E_SDK_DIR   — scratch dir for the SQLite cache + cli
#                         build artefacts (default a fresh mktemp).
#
# Requires: cargo (Rust toolchain), jq.

set -u

API="${KMAIL_API_URL:-http://localhost:8088}"
TOK="${KMAIL_DEV_BEARER:-kmail-dev}"
# If the caller passed `KMAIL_E2E_SDK_DIR` explicitly we leave
# their scratch dir alone on exit so they can poke at the
# resulting SQLite cache after a failing run. If we minted a
# fresh `mktemp -d` ourselves, we own it and clean it up on EXIT
# so repeated local invocations don't leak temp dirs.
if [ -n "${KMAIL_E2E_SDK_DIR:-}" ]; then
  SCRATCH="${KMAIL_E2E_SDK_DIR}"
else
  SCRATCH="$(mktemp -d)"
  trap 'rm -rf "${SCRATCH}"' EXIT INT TERM
fi
DB="${SCRATCH}/kmail.db"
LOG_DIR="${SCRATCH}/logs"
mkdir -p "${LOG_DIR}"
FAIL=0

# Resolve the kmail repo root so the script is reusable from
# anywhere (CI runs it from $GITHUB_WORKSPACE, dev runs it from
# the repo root). The `sdk/` Cargo workspace is relative to the
# repo root by construction.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SDK_DIR="${REPO_ROOT}/sdk"

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
    printf 'kmail-e2e-sdk: %s required\n' "$1" 1>&2
    exit 2
  }
}
require cargo
require jq

# ---------------------------------------------------------------
# 0. Build kmail-cli once
# ---------------------------------------------------------------
# A single release build up-front; subsequent `kmail` invocations
# reuse the cached binary. Building once keeps the per-step
# latency bounded so the rest of the script reads like a sequence
# of fast probes, not a build pipeline.
step '0. cargo build --release -p kmail-cli'
CLI_BIN="${SDK_DIR}/target/release/kmail"
BUILD_LOG="${LOG_DIR}/cargo-build.log"
# Cargo's stdout/stderr is captured to ${BUILD_LOG} instead of
# /dev/null so a build failure leaves a forensic trail. In CI,
# `.github/workflows/sdk-nightly.yml` step "Dump SDK e2e logs on
# failure" tails this file alongside kmail-api / docker logs;
# for local runs the cleanup trap above keeps the file alive
# only when the caller provided `KMAIL_E2E_SDK_DIR` (i.e. they
# explicitly opted into a persistent scratch dir).
if ( cd "${SDK_DIR}" && cargo build --release -p kmail-cli >"${BUILD_LOG}" 2>&1 ); then
  if [ -x "${CLI_BIN}" ]; then ok
  else fail "kmail binary missing at ${CLI_BIN} after release build (see ${BUILD_LOG})"; fi
else
  fail "cargo build -p kmail-cli (release) exited non-zero (see ${BUILD_LOG})"
  # Surface the tail of the build log so the failure mode is
  # visible directly in the run output even before the workflow's
  # log-dump step fires. 50 lines is enough for most rustc
  # error[E####] blocks without overwhelming the run log.
  tail -n 50 "${BUILD_LOG}" 1>&2 || true
fi

# Every subsequent step short-circuits on a missing binary instead
# of dragging the whole probe sequence to confusing curl-style
# errors against an unbuilt CLI.
if [ ! -x "${CLI_BIN}" ]; then
  echo "kmail-e2e-sdk: CLI binary missing; skipping remaining steps" 1>&2
  exit 2
fi

# ---------------------------------------------------------------
# 1. Session discovery
# ---------------------------------------------------------------
# `kmail session` → `KMailClient::discover_session()` which fires
# a single GET against `${API}/jmap/session`. We parse the JSON to
# confirm the SDK round-tripped the response into a `JmapSession`
# document (`username` + at least one entry under `accounts`).
# `--token "$TOK"` exercises the dev-bypass branch of the auth
# middleware; the assertion stays valid against a real OIDC
# deployment too.
step '1. kmail session (JMAP /jmap/session discovery)'
SESSION_JSON=$("${CLI_BIN}" session --bff "${API}" --token "${TOK}" 2>/dev/null || echo '{}')
USERNAME=$(printf '%s' "${SESSION_JSON}" | jq -r '.username // empty')
if [ -n "${USERNAME}" ]; then ok
else fail "session JSON missing .username field"; fi
ACCOUNTS_N=$(printf '%s' "${SESSION_JSON}" | jq -r '.accounts | length')
if [ "${ACCOUNTS_N:-0}" -gt 0 ] 2>/dev/null; then ok
else fail "session JSON has no accounts (.accounts is empty)"; fi

# ---------------------------------------------------------------
# 2. Delta-pull sync
# ---------------------------------------------------------------
# `kmail sync` exercises the full Mailbox/get + Email/query +
# Email/get pipeline (the cold-start path until #44's
# `bootstrap_sync` lands). The nightly workflow seeds a default
# dev tenant + user via `scripts/seed-dev-tenant.sql`, and the
# Stalwart side's `scripts/stalwart-init.sh` bootstraps the
# `kmail-dev` account with the standard role mailboxes (Inbox,
# Drafts, Sent, Trash, Junk). The cold-start sync therefore MUST
# observe at least one mailbox upsert — a zero count signals a
# regression somewhere in the JMAP proxy → SDK pipeline, not
# just an empty seed, so we assert `> 0` rather than the weaker
# `>= 0` (which would pass even when sync returned nothing). The
# `-1` sentinel from jq lets us distinguish 'field missing' from
# 'field present but zero' in the failure message.
step '2. kmail sync (delta-pull)'
SYNC_JSON=$("${CLI_BIN}" sync --bff "${API}" --token "${TOK}" --db "${DB}" 2>/dev/null || echo '{}')
MBX_N=$(printf '%s' "${SYNC_JSON}" | jq -r '.mailboxesUpserted // -1')
if [ "${MBX_N:-0}" -gt 0 ] 2>/dev/null; then ok
elif [ "${MBX_N:-0}" -eq 0 ] 2>/dev/null; then
  fail "sync summary reports mailboxesUpserted=0 (expected the seeded Stalwart account to surface at least Inbox)"
else
  fail "sync summary missing mailboxesUpserted field"
fi

# ---------------------------------------------------------------
# 3. Local SQLite read paths
# ---------------------------------------------------------------
# `kmail mailboxes` reads the local SQLite cache populated by the
# prior `kmail sync`. A JSON array reply (even an empty one)
# confirms the schema migrated cleanly. Coupling the assertion to
# "array, not error" rather than "non-empty array" keeps the test
# robust to the seed-data evolving in the dev compose stack.
step '3. kmail mailboxes (local SQLite)'
MBX_JSON=$("${CLI_BIN}" mailboxes --db "${DB}" 2>/dev/null || echo 'null')
if printf '%s' "${MBX_JSON}" | jq -e 'type == "array"' >/dev/null 2>&1; then ok
else fail "mailboxes output is not a JSON array"; fi

# ---------------------------------------------------------------
# 4. Doctor (schema + sqlite version)
# ---------------------------------------------------------------
# `kmail doctor` is the SDK's self-diagnostic surface; it prints
# the schema version, the SQLite version it was linked against,
# and row counts. A non-zero schemaVersion is the cheapest
# invariant to confirm migrations ran end-to-end.
step '4. kmail doctor (schema + sqlite version)'
DOCTOR_JSON=$("${CLI_BIN}" doctor --db "${DB}" 2>/dev/null || echo '{}')
SCHEMA_V=$(printf '%s' "${DOCTOR_JSON}" | jq -r '.schemaVersion // 0')
if [ "${SCHEMA_V:-0}" -gt 0 ] 2>/dev/null; then ok
else fail "doctor reports schemaVersion=${SCHEMA_V} (expected > 0)"; fi
SQLITE_V=$(printf '%s' "${DOCTOR_JSON}" | jq -r '.sqliteVersion // empty')
if [ -n "${SQLITE_V}" ]; then ok
else fail "doctor missing sqliteVersion"; fi

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
if [ "${FAIL}" -eq 0 ]; then
  printf '\nkmail-e2e-sdk: ALL OK\n'
  exit 0
fi
printf '\nkmail-e2e-sdk: %d step(s) failed\n' "${FAIL}" 1>&2
exit 1
