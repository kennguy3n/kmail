#!/usr/bin/env sh
# KMail — CalDAV compatibility test.
#
# Verifies that a Stalwart v0.16.0 deployment accepts CalDAV
# requests on the expected HTTP endpoint, advertises the
# `calendar-access` compliance class in OPTIONS, enumerates the
# authenticated user's calendar collection via PROPFIND, and
# round-trips a VEVENT through PUT → GET → DELETE.
#
# Not a substitute for the full Apple Calendar / Thunderbird
# compatibility matrix in `docs/COMPATIBILITY.md`; this script
# only asserts the protocol surface those clients depend on.
#
# Inputs (all have sensible compose-stack defaults):
#   CALDAV_URL       — base URL including /dav/calendars/ path
#                      (default http://localhost:8080/dav/calendars/)
#   CALDAV_USER      — full email address
#   CALDAV_PASS      — password for CALDAV_USER
#   CALDAV_CALENDAR  — collection name within the user's home
#                      (default "default")
#
# Usage:
#   CALDAV_USER=admin@kmail.dev CALDAV_PASS=secret \
#     ./scripts/test-caldav.sh

set -eu

CALDAV_URL=${CALDAV_URL:-http://localhost:8080/dav/calendars/}
CALDAV_USER=${CALDAV_USER:-}
CALDAV_PASS=${CALDAV_PASS:-}
CALDAV_CALENDAR=${CALDAV_CALENDAR:-default}

log()  { printf '[test-caldav] %s\n' "$*"; }
fail() { printf '[test-caldav] FAIL: %s\n' "$*" >&2; exit 1; }

if [ -z "$CALDAV_USER" ] || [ -z "$CALDAV_PASS" ]; then
  fail "CALDAV_USER and CALDAV_PASS must be set"
fi
command -v curl >/dev/null 2>&1 || fail "curl is required"

# Strip any trailing slash + rebuild predictable paths.
BASE=$(printf '%s' "$CALDAV_URL" | sed 's:/*$::')
HOME_URL="${BASE}/${CALDAV_USER}/"
CAL_URL="${HOME_URL}${CALDAV_CALENDAR}/"
UID_STR="kmail-compat-$(date +%s)-$$"
EVT_URL="${CAL_URL}${UID_STR}.ics"

curl_auth() {
  curl -sS -u "${CALDAV_USER}:${CALDAV_PASS}" "$@"
}

# ------------------------------------------------------------------
# 1. OPTIONS against the calendar home — announce compliance class.
# ------------------------------------------------------------------
log "OPTIONS ${HOME_URL}"
opts_hdr=$(curl_auth -i -X OPTIONS "$HOME_URL" -o /dev/null -D - -w '%{http_code}\n')
printf '%s' "$opts_hdr" | grep -qi '^dav:.*calendar-access' \
  || fail "OPTIONS did not announce DAV: calendar-access"
log "OPTIONS OK — calendar-access compliance class present"

# ------------------------------------------------------------------
# 2. PROPFIND Depth:0 — assert the calendar-home-set is resolvable.
# ------------------------------------------------------------------
PROPFIND_HOME=$(cat <<'XML'
<?xml version="1.0" encoding="utf-8" ?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:prop>
    <d:resourcetype/>
    <d:displayname/>
    <c:calendar-home-set/>
  </d:prop>
</d:propfind>
XML
)
log "PROPFIND Depth:0 ${HOME_URL}"
home_xml=$(curl_auth -X PROPFIND \
  -H 'Depth: 0' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data "$PROPFIND_HOME" \
  "$HOME_URL")
printf '%s' "$home_xml" | grep -qi '<d:multistatus\|<multistatus\|207 ' \
  || { printf '%s\n' "$home_xml" >&2; fail "PROPFIND Depth:0 did not return a multistatus"; }
log "PROPFIND Depth:0 OK"

# ------------------------------------------------------------------
# 3. PROPFIND Depth:1 — list calendar collections under the home.
# ------------------------------------------------------------------
PROPFIND_LIST=$(cat <<'XML'
<?xml version="1.0" encoding="utf-8" ?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">
  <d:prop>
    <d:resourcetype/>
    <d:displayname/>
    <c:supported-calendar-component-set/>
  </d:prop>
</d:propfind>
XML
)
log "PROPFIND Depth:1 ${HOME_URL}"
list_xml=$(curl_auth -X PROPFIND \
  -H 'Depth: 1' \
  -H 'Content-Type: application/xml; charset=utf-8' \
  --data "$PROPFIND_LIST" \
  "$HOME_URL")
printf '%s' "$list_xml" | grep -qi 'calendar' \
  || { printf '%s\n' "$list_xml" >&2; fail "PROPFIND Depth:1 did not list a calendar collection"; }
log "PROPFIND Depth:1 OK"

# ------------------------------------------------------------------
# 4. PUT a test VEVENT, then GET it back and verify SUMMARY.
# ------------------------------------------------------------------
START=$(date -u "+%Y%m%dT%H%M%SZ")
END_TS=$(( $(date +%s) + 3600 ))
# `date -d` is GNU-only; `date -u -r <epoch>` is BSD / macOS. Try GNU first.
if END=$(date -u -d "@${END_TS}" "+%Y%m%dT%H%M%SZ" 2>/dev/null); then
  :
else
  END=$(date -u -r "${END_TS}" "+%Y%m%dT%H%M%SZ")
fi
DTSTAMP=$START

SUMMARY="KMail CalDAV compat ${UID_STR}"

VEVENT=$(cat <<ICS
BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//kmail//compat-test//EN
BEGIN:VEVENT
UID:${UID_STR}
DTSTAMP:${DTSTAMP}
DTSTART:${START}
DTEND:${END}
SUMMARY:${SUMMARY}
DESCRIPTION:Round-trip test from scripts/test-caldav.sh
END:VEVENT
END:VCALENDAR
ICS
)

log "PUT ${EVT_URL}"
put_code=$(curl_auth -X PUT \
  -H 'Content-Type: text/calendar; charset=utf-8' \
  --data "$VEVENT" \
  -o /dev/null -w '%{http_code}' \
  "$EVT_URL")
case "$put_code" in
  201|204) log "PUT returned ${put_code}" ;;
  *) fail "PUT returned unexpected status: ${put_code}" ;;
esac

log "GET ${EVT_URL}"
get_body=$(curl_auth -X GET "$EVT_URL")
printf '%s' "$get_body" | grep -q "SUMMARY:${SUMMARY}" \
  || { printf '%s\n' "$get_body" >&2; fail "GET did not return the SUMMARY we PUT"; }
printf '%s' "$get_body" | grep -q "UID:${UID_STR}" \
  || fail "GET body is missing the expected UID"
log "GET OK — round-trip verified"

# ------------------------------------------------------------------
# 5. DELETE the test event so repeated runs stay clean.
# ------------------------------------------------------------------
log "DELETE ${EVT_URL}"
del_code=$(curl_auth -X DELETE -o /dev/null -w '%{http_code}' "$EVT_URL")
case "$del_code" in
  200|202|204) log "DELETE returned ${del_code}" ;;
  *) fail "DELETE returned unexpected status: ${del_code}" ;;
esac

log "CalDAV round-trip OK"
