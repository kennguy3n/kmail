#!/usr/bin/env bash
# Capture demo screenshots end-to-end:
#
#   1. Start the Vite dev server with MSW mocking enabled.
#   2. Wait until it answers on http://localhost:5173/.
#   3. Run scripts/capture-screenshots.mjs.
#   4. Stop the Vite dev server.
#   5. Verify every expected PNG landed in docs/screenshots/.
#
# Run from the repo root: `./scripts/capture-screenshots-with-mock.sh`
# or via `make screenshots`.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEB_DIR="${REPO_ROOT}/web"
OUT_DIR="${REPO_ROOT}/docs/screenshots"
PORT="${KMAIL_VITE_PORT:-5173}"
URL="http://localhost:${PORT}"

cleanup() {
  if [[ -n "${VITE_PID:-}" ]] && kill -0 "${VITE_PID}" 2>/dev/null; then
    echo "Stopping Vite (pid ${VITE_PID})…"
    kill "${VITE_PID}" 2>/dev/null || true
    wait "${VITE_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "Starting Vite with VITE_MOCK_API=true on port ${PORT}…"
(
  cd "${WEB_DIR}"
  VITE_MOCK_API=true npx vite --port "${PORT}" --strictPort >/tmp/kmail-vite.log 2>&1
) &
VITE_PID=$!

echo "Waiting for ${URL} (pid ${VITE_PID})…"
for i in $(seq 1 60); do
  if curl -fsS "${URL}/" >/dev/null 2>&1; then
    echo "Vite is up."
    break
  fi
  if ! kill -0 "${VITE_PID}" 2>/dev/null; then
    echo "Vite exited early. Last log lines:" >&2
    tail -n 50 /tmp/kmail-vite.log >&2 || true
    exit 1
  fi
  sleep 1
  if [[ "${i}" == "60" ]]; then
    echo "Vite did not become ready in 60 seconds." >&2
    tail -n 50 /tmp/kmail-vite.log >&2 || true
    exit 1
  fi
done

echo "Capturing screenshots…"
# Forward the resolved Vite URL so a non-default KMAIL_VITE_PORT
# also reaches the Node script — otherwise it falls back to the
# hardcoded :5173 default and Playwright would screenshot nothing.
(cd "${REPO_ROOT}" && KMAIL_DEV_URL="${URL}" node scripts/capture-screenshots.mjs)

echo "Verifying expected PNGs in ${OUT_DIR}…"
EXPECTED=(
  01-mail-inbox.png 02-compose.png 03-vault.png 04-shared-inbox.png
  05-protected-folders.png 06-calendar.png 07-event-create.png
  08-shared-calendars.png 09-contacts.png 10-secure-portal.png
  11-domain-admin.png 12-dns-wizard.png 13-user-admin.png
  14-pricing-admin.png 15-pricing-page.png 16-dkim-admin.png
  17-sieve-admin.png 18-security-settings.png 19-search-admin.png
  20-slo-admin.png 21-onboarding.png 22-retention-admin.png
  23-webhook-admin.png 24-audit-admin.png 25-billing-admin.png
  26-cmk-admin.png 27-scim-admin.png 28-export-admin.png
)
MISSING=0
for f in "${EXPECTED[@]}"; do
  if [[ ! -s "${OUT_DIR}/${f}" ]]; then
    echo "missing or empty: ${OUT_DIR}/${f}" >&2
    MISSING=$((MISSING + 1))
  fi
done
if (( MISSING > 0 )); then
  echo "${MISSING} screenshot(s) missing." >&2
  exit 1
fi

echo "All ${#EXPECTED[@]} screenshots present in ${OUT_DIR}/."
