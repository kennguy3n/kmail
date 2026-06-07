#!/usr/bin/env bash
# ---------------------------------------------------------------------
# helm-sync-dashboards.sh — mirror the canonical Grafana dashboards
# into the Helm chart so the dashboard ConfigMap template can embed
# them.
#
# WHY THIS EXISTS
# ---------------
# The canonical Grafana dashboards live in deploy/grafana/dashboards/
# (mounted directly by the docker-compose dev stack). The Helm chart's
# grafana-dashboards-configmap.yaml embeds those same JSON files via
# Helm's `.Files.Glob`, which can ONLY read files inside the chart
# root. So the chart keeps a mirror under deploy/helm/kmail/dashboards/.
#
# This script keeps the mirror in sync with the canonical source. Run
# it after editing any dashboard:
#
#     make helm-sync-dashboards
#
# Pass --check to fail (non-zero exit) if the mirror is stale instead
# of rewriting it — suitable for a CI guard against drift.
# ---------------------------------------------------------------------
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="${repo_root}/deploy/grafana/dashboards"
dst="${repo_root}/deploy/helm/kmail/dashboards"

check_only=0
if [[ "${1:-}" == "--check" ]]; then
  check_only=1
fi

if [[ ! -d "${src}" ]]; then
  echo "error: source dashboards dir not found: ${src}" >&2
  exit 1
fi

mkdir -p "${dst}"

drift=0
shopt -s nullglob
for f in "${src}"/*.json; do
  name="$(basename "${f}")"
  target="${dst}/${name}"
  if [[ ! -f "${target}" ]] || ! cmp -s "${f}" "${target}"; then
    drift=1
    if [[ "${check_only}" -eq 1 ]]; then
      echo "DRIFT: ${target} is out of sync with ${f}" >&2
    else
      cp "${f}" "${target}"
      echo "synced: ${name}"
    fi
  fi
done

# Remove mirror files whose canonical source was deleted.
for f in "${dst}"/*.json; do
  name="$(basename "${f}")"
  if [[ ! -f "${src}/${name}" ]]; then
    drift=1
    if [[ "${check_only}" -eq 1 ]]; then
      echo "DRIFT: ${f} has no canonical source in ${src}" >&2
    else
      rm "${f}"
      echo "removed stale mirror: ${name}"
    fi
  fi
done

if [[ "${check_only}" -eq 1 && "${drift}" -eq 1 ]]; then
  echo "" >&2
  echo "Grafana dashboard mirror is stale. Run: make helm-sync-dashboards" >&2
  exit 1
fi

if [[ "${drift}" -eq 0 ]]; then
  echo "dashboards already in sync"
fi
