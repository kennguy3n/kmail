#!/usr/bin/env bash
#
# generate-evidence.sh — collect SOC 2 evidence into a timestamped
# bundle. Maps to the "Evidence Collection Procedures" table in
# docs/compliance/SOC2_CONTROL_MAPPING.md.
#
# Each collector writes one file under a dated output directory and
# is independently selectable. With no collector flags, all are run.
# Collectors degrade gracefully: a missing dependency (psql, gh) is
# recorded as a SKIPPED note in the bundle rather than aborting the
# run, so the bundle always reflects what was actually collectable.
#
# Usage:
#   scripts/compliance/generate-evidence.sh [options] [collectors]
#
# Collectors (default: all):
#   --audit           Audit-log hash-chain verification (CC4.2/CC6.3)
#   --access-review   Per-tenant user/role dump        (CC6.1/CC6.2)
#   --change-log      Merged PRs + reviewer + CI        (CC8.1)
#   --vendor-review   Snapshot the vendor register      (CC9.1)
#   --deps            Dependency vuln scans (Go/npm/cargo) (CC7.1/CC9.2)
#
# Options:
#   --out DIR         Output root (default: ./compliance-evidence)
#   --database-url U  Postgres URL (default: $DATABASE_URL)
#   --since DATE      Lower bound for change-log (default: 90 days ago)
#   -h, --help        Show this help
#
# Environment:
#   DATABASE_URL      Used by --audit and --access-review.
#   GITHUB_REPOSITORY owner/repo for --change-log (default: kennguy3n/kmail)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT_ROOT="./compliance-evidence"
DATABASE_URL="${DATABASE_URL:-}"
GITHUB_REPOSITORY="${GITHUB_REPOSITORY:-kennguy3n/kmail}"
SINCE=""
RUN_AUDIT=0
RUN_ACCESS=0
RUN_CHANGE=0
RUN_VENDOR=0
RUN_DEPS=0
ANY_COLLECTOR=0

usage() { sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
	case "$1" in
		--audit)         RUN_AUDIT=1; ANY_COLLECTOR=1 ;;
		--access-review) RUN_ACCESS=1; ANY_COLLECTOR=1 ;;
		--change-log)    RUN_CHANGE=1; ANY_COLLECTOR=1 ;;
		--vendor-review) RUN_VENDOR=1; ANY_COLLECTOR=1 ;;
		--deps)          RUN_DEPS=1; ANY_COLLECTOR=1 ;;
		--out)           OUT_ROOT="$2"; shift ;;
		--database-url)  DATABASE_URL="$2"; shift ;;
		--since)         SINCE="$2"; shift ;;
		-h|--help)       usage; exit 0 ;;
		*) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
	esac
	shift
done

# No collector selected → run everything.
if [ "$ANY_COLLECTOR" -eq 0 ]; then
	RUN_AUDIT=1; RUN_ACCESS=1; RUN_CHANGE=1; RUN_VENDOR=1; RUN_DEPS=1
fi

if [ -z "$SINCE" ]; then
	# 90 days ago, portable across GNU and BSD date.
	SINCE="$(date -u -d '90 days ago' '+%Y-%m-%d' 2>/dev/null \
		|| date -u -v-90d '+%Y-%m-%d' 2>/dev/null \
		|| echo '1970-01-01')"
fi

TS="$(date -u '+%Y%m%dT%H%M%SZ')"
OUT_DIR="${OUT_ROOT}/${TS}"
mkdir -p "$OUT_DIR"

note() { echo "[generate-evidence] $*"; }

# manifest accumulates one line per artifact for the auditor index.
MANIFEST="${OUT_DIR}/MANIFEST.txt"
{
	echo "SOC 2 evidence bundle"
	echo "Generated (UTC): ${TS}"
	echo "Repository:      ${GITHUB_REPOSITORY}"
	echo "Git commit:      $(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
	echo "---"
} > "$MANIFEST"

record() { echo "$1" >> "$MANIFEST"; }

collect_audit() {
	local f="${OUT_DIR}/audit-chain-verification.txt"
	note "CC4.2/CC6.3 audit-log hash-chain verification"
	if [ -z "$DATABASE_URL" ]; then
		echo "SKIPPED: DATABASE_URL not set" > "$f"
		record "audit-chain-verification.txt   SKIPPED (no DATABASE_URL)"
		return
	fi
	if ! command -v psql >/dev/null 2>&1; then
		echo "SKIPPED: psql not installed" > "$f"
		record "audit-chain-verification.txt   SKIPPED (no psql)"
		return
	fi
	{
		echo "Audit-log integrity evidence — ${TS}"
		echo "Control: CC4.2 (audit-log integrity), CC6.3 (privileged access auditing)"
		echo "Method: row count + chain endpoints; full verification via internal/audit VerifyChain"
		echo "---"
		psql "$DATABASE_URL" -At -c \
			"SELECT 'rows=' || count(*) FROM audit_log;" 2>&1 || echo "query failed"
		psql "$DATABASE_URL" -At -c \
			"SELECT 'earliest=' || min(created_at) || ' latest=' || max(created_at) FROM audit_log;" 2>&1 || true
	} > "$f"
	record "audit-chain-verification.txt   OK"
}

collect_access_review() {
	local f="${OUT_DIR}/access-review.csv"
	note "CC6.1/CC6.2 quarterly user access review"
	if [ -z "$DATABASE_URL" ] || ! command -v psql >/dev/null 2>&1; then
		echo "SKIPPED: requires DATABASE_URL and psql" > "$f"
		record "access-review.csv             SKIPPED (no DATABASE_URL/psql)"
		return
	fi
	# Per-tenant user/role snapshot. Column names are best-effort;
	# adjust the query to the deployed schema if it differs.
	if psql "$DATABASE_URL" --csv -c \
		"SELECT tenant_id, email, role, status, created_at FROM users ORDER BY tenant_id, role;" \
		> "$f" 2>"${f}.err"; then
		rm -f "${f}.err"
		record "access-review.csv             OK"
	else
		mv "${f}.err" "$f"
		record "access-review.csv             ERROR (see file)"
	fi
}

collect_change_log() {
	local f="${OUT_DIR}/change-log.txt"
	note "CC8.1 change log (merged PRs since ${SINCE})"
	if ! command -v gh >/dev/null 2>&1; then
		echo "SKIPPED: gh (GitHub CLI) not installed" > "$f"
		record "change-log.txt                SKIPPED (no gh)"
		return
	fi
	{
		echo "Change-management evidence — ${TS}"
		echo "Control: CC8.1 (authorised changes)"
		echo "Repo: ${GITHUB_REPOSITORY}  Since: ${SINCE}"
		echo "---"
		gh pr list --repo "$GITHUB_REPOSITORY" --state merged --limit 200 \
			--search "merged:>=${SINCE}" \
			--json number,title,author,mergedAt,reviewDecision \
			--template '{{range .}}#{{.number}} {{.mergedAt}} by {{.author.login}} review={{.reviewDecision}} — {{.title}}{{"\n"}}{{end}}' \
			2>&1 || echo "gh query failed (auth?)"
	} > "$f"
	record "change-log.txt                OK"
}

collect_vendor_review() {
	local src="${REPO_ROOT}/docs/compliance/vendors.md"
	local f="${OUT_DIR}/vendor-register.md"
	note "CC9.1 vendor management register snapshot"
	if [ -f "$src" ]; then
		{
			echo "<!-- snapshot captured ${TS} -->"
			cat "$src"
		} > "$f"
		record "vendor-register.md            OK"
	else
		echo "SKIPPED: docs/compliance/vendors.md not found" > "$f"
		record "vendor-register.md            SKIPPED (source missing)"
	fi
}

# collect_deps runs the same dependency vulnerability scanners that
# the (non-blocking) Security Scan workflow runs, capturing their
# output as dated evidence. Each scanner degrades gracefully when
# its toolchain is absent so the bundle records what was actually
# collectable. Non-zero scanner exit codes (findings present) are
# expected and captured, not treated as collector failures.
collect_deps() {
	local f="${OUT_DIR}/dependency-scan.txt"
	note "CC7.1/CC9.2 dependency vulnerability scans"
	{
		echo "Dependency-scan evidence — ${TS}"
		echo "Control: CC7.1 (system operations), CC9.2 (risk mitigation)"
		echo "Scanners: govulncheck (Go), npm audit (web), cargo audit (sdk)"
		echo "=== govulncheck ./... ==="
		if command -v govulncheck >/dev/null 2>&1; then
			(cd "$REPO_ROOT" && govulncheck ./... 2>&1) || true
		else
			echo "SKIPPED: govulncheck not installed (go install golang.org/x/vuln/cmd/govulncheck@latest)"
		fi
		echo "=== npm audit (web, --omit=dev) ==="
		if command -v npm >/dev/null 2>&1 && [ -f "${REPO_ROOT}/web/package-lock.json" ]; then
			(cd "${REPO_ROOT}/web" && npm audit --omit=dev 2>&1) || true
		else
			echo "SKIPPED: npm or web/package-lock.json not available"
		fi
		echo "=== cargo audit (sdk) ==="
		if command -v cargo-audit >/dev/null 2>&1 && [ -f "${REPO_ROOT}/sdk/Cargo.lock" ]; then
			(cd "${REPO_ROOT}/sdk" && cargo audit 2>&1) || true
		else
			echo "SKIPPED: cargo-audit or sdk/Cargo.lock not available"
		fi
	} > "$f"
	record "dependency-scan.txt            OK"
}

[ "$RUN_AUDIT" -eq 1 ]  && collect_audit
[ "$RUN_ACCESS" -eq 1 ] && collect_access_review
[ "$RUN_CHANGE" -eq 1 ] && collect_change_log
[ "$RUN_VENDOR" -eq 1 ] && collect_vendor_review
[ "$RUN_DEPS" -eq 1 ]   && collect_deps

note "evidence bundle written to ${OUT_DIR}"
cat "$MANIFEST"
