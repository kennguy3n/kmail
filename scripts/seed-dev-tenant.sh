#!/usr/bin/env sh
# KMail — dev-stack default tenant + user seeder.
#
# Resolves the kmail-dev mailbox principal's *server-assigned* JMAP
# account id from Stalwart at run time (by the principal's stable
# name, via the `x:Account/query` management method) and applies
# `scripts/seed-dev-tenant.sql` with that id bound to the
# `dev_stalwart_account_id` psql variable.
#
# Why a wrapper instead of a hard-coded id in the SQL: Stalwart assigns
# principal ids in creation order, so the kmail-dev account's id is an
# implementation detail of `scripts/stalwart-init.sh`'s provisioning
# order and the pinned Stalwart version. Hard-coding it couples the
# fixture to that order — a version bump or a provisioning-order change
# would silently desync the seeded `stalwart_account_id` from the real
# principal and every proxied JMAP call would 404. Resolving by name
# removes that coupling and keeps the seed self-correcting on re-run.
#
# Environment (all optional; defaults target the docker-compose dev
# stack as exposed to the host / CI runner):
#
#   DATABASE_URL                Postgres DSN for the seed
#                               (default: postgresql://kmail:kmail@localhost:5432/kmail)
#   KMAIL_STALWART_ADMIN_URL    Stalwart admin base URL; falls back to
#                               STALWART_URL, then http://localhost:8080
#   KMAIL_STALWART_ADMIN_USER   Stalwart recovery-admin user (default: admin)
#   KMAIL_STALWART_ADMIN_PASS   Stalwart recovery-admin password (default: kmail-dev)
#   KMAIL_DEV_ACCOUNT_NAME      principal name to resolve (default: kmail-dev)
#
# Fails loud (non-zero exit) if the principal id cannot be resolved, so
# a misprovisioned Stalwart surfaces here rather than as a confusing
# downstream accountNotFound 404.
set -eu

DATABASE_URL=${DATABASE_URL:-postgresql://kmail:kmail@localhost:5432/kmail}
ADMIN_URL=${KMAIL_STALWART_ADMIN_URL:-${STALWART_URL:-http://localhost:8080}}
ADMIN_USER=${KMAIL_STALWART_ADMIN_USER:-admin}
ADMIN_PASS=${KMAIL_STALWART_ADMIN_PASS:-kmail-dev}
DEV_ACCOUNT_NAME=${KMAIL_DEV_ACCOUNT_NAME:-kmail-dev}

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

# Trim any trailing slash so "${ADMIN_URL}/jmap" is well-formed.
ADMIN_URL=$(printf '%s' "$ADMIN_URL" | sed 's:/*$::')

# Resolve the principal's JMAP id by its stable name. accountId is "" —
# the query is global to the registry and does not target a mailbox.
query=$(printf '{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["x:Account/query",{"accountId":"","filter":{"name":"%s"}},"c1"]]}' "$DEV_ACCOUNT_NAME")
response=$(curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" \
    -H 'Content-Type: application/json' \
    -X POST "${ADMIN_URL}/jmap" \
    -d "$query")

# Extract the first id from "ids":["<id>"] (value-only sed keeps us free
# of a jq dependency, matching scripts/stalwart-init.sh).
account_id=$(printf '%s' "$response" | sed -n 's/.*"ids":\["\([^"]*\)".*/\1/p')
if [ -z "$account_id" ]; then
    echo "seed-dev-tenant: could not resolve JMAP account id for principal '${DEV_ACCOUNT_NAME}' at ${ADMIN_URL}" >&2
    echo "seed-dev-tenant: x:Account/query response was: ${response}" >&2
    echo "seed-dev-tenant: is scripts/stalwart-init.sh provisioning the principal? (it runs as the stalwart-init one-shot)" >&2
    exit 1
fi

echo "seed-dev-tenant: resolved principal '${DEV_ACCOUNT_NAME}' -> JMAP account id '${account_id}'"

exec psql "$DATABASE_URL" \
    -v ON_ERROR_STOP=1 \
    -v dev_stalwart_account_id="$account_id" \
    -f "${SCRIPT_DIR}/seed-dev-tenant.sql"
