#!/usr/bin/env bash
# KMail — shard provisioning wrapper (WS4 Task 3).
#
# Stands up one mailbox shard via the `deploy/terraform/shard/` module
# and prints the resulting shard's connection details as a single JSON
# object on stdout, in the shape `internal/tenant.ExecShardProvisioner`
# parses:
#
#     {"name":"...","stalwart_url":"...","postgres_dsn":"...","max_mailboxes":10000}
#
# The control plane's auto-provisioner invokes this script with the
# desired shard name as the first argument when active capacity crosses
# the utilisation threshold (KMAIL_SHARD_AUTOPROVISION_THRESHOLD).
#
# Usage:
#     ./scripts/provision-shard.sh <shard-name>
#
# Environment:
#     KMAIL_SHARD_TF_DIR   Terraform module dir (default: deploy/terraform/shard)
#     KMAIL_SHARD_MAX_MAILBOXES  capacity hint (default: 10000)
#     TF_VAR_region, TF_VAR_*    forwarded to terraform as usual
#
# Contract: ALL human-readable progress goes to stderr; stdout carries
# ONLY the final JSON so the caller can parse it verbatim.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF_DIR="${KMAIL_SHARD_TF_DIR:-${REPO_ROOT}/deploy/terraform/shard}"
MAX_MAILBOXES="${KMAIL_SHARD_MAX_MAILBOXES:-10000}"

log() { printf '[provision-shard] %s\n' "$*" >&2; }

SHARD_NAME="${1:-}"
if [[ -z "${SHARD_NAME}" ]]; then
    log "error: shard name required (usage: provision-shard.sh <shard-name>)"
    exit 2
fi

if ! command -v terraform >/dev/null 2>&1; then
    log "error: terraform not found on PATH"
    exit 3
fi

log "provisioning shard '${SHARD_NAME}' via ${TF_DIR}"

# Per-shard workspace keeps state isolated so concurrent provisions of
# different shards don't clobber each other.
terraform -chdir="${TF_DIR}" init -input=false >&2
terraform -chdir="${TF_DIR}" workspace select "${SHARD_NAME}" >&2 2>/dev/null \
    || terraform -chdir="${TF_DIR}" workspace new "${SHARD_NAME}" >&2

terraform -chdir="${TF_DIR}" apply -input=false -auto-approve \
    -var "shard_name=${SHARD_NAME}" \
    -var "max_mailboxes=${MAX_MAILBOXES}" >&2

# Emit ONLY the JSON contract on stdout.
terraform -chdir="${TF_DIR}" output -json shard

log "shard '${SHARD_NAME}' provisioned"
