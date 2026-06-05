#!/usr/bin/env bash
#
# zap-baseline.sh — run the OWASP ZAP baseline scan against a target
# using the official ZAP container, applying zap-baseline.conf.
#
# The baseline scan is passive + safe (no active attack payloads), so
# it is appropriate to run against a staging deployment and as a
# non-blocking CI job. It surfaces missing security headers, info
# leaks, and obvious misconfigurations.
#
# Usage:
#   TARGET=https://staging.kmail.example scripts/security/zap-baseline.sh
#
# Required environment:
#   TARGET   Base URL to scan.
# Optional:
#   ZAP_IMAGE   Container image (default: ghcr.io/zaproxy/zaproxy:stable)
#   REPORT_DIR  Where to write the HTML/JSON report (default: ./zap-report)
#
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -z "${TARGET:-}" ]; then
	echo "SKIPPED: set TARGET to the base URL to scan" >&2
	exit 0
fi

ZAP_IMAGE="${ZAP_IMAGE:-ghcr.io/zaproxy/zaproxy:stable}"
REPORT_DIR="${REPORT_DIR:-./zap-report}"
mkdir -p "$REPORT_DIR"

if ! command -v docker >/dev/null 2>&1; then
	echo "SKIPPED: docker not available (needed to run ${ZAP_IMAGE})" >&2
	exit 0
fi

echo "Running ZAP baseline scan against ${TARGET}"
# Mount the config + report dir into the container. zap-baseline.py
# returns non-zero when a FAIL-level rule trips; callers decide
# whether that gates the pipeline.
docker run --rm \
	-v "${HERE}:/zap/cfg:ro" \
	-v "$(cd "$REPORT_DIR" && pwd):/zap/wrk:rw" \
	"$ZAP_IMAGE" \
	zap-baseline.py \
	-t "$TARGET" \
	-c /zap/cfg/zap-baseline.conf \
	-r zap-report.html \
	-J zap-report.json \
	-I
