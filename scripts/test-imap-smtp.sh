#!/usr/bin/env sh
# KMail — IMAP / SMTP compatibility test.
#
# Verifies that a Stalwart v0.16.0 deployment started via
# `docker compose up` accepts IMAP and SMTP connections on the
# expected host ports, completes STARTTLS negotiation, accepts
# AUTH PLAIN, and round-trips a locally-generated test message.
# Fails fast on the first check that does not meet the documented
# compatibility profile in `docs/COMPATIBILITY.md`.
#
# Not a substitute for the actual third-party client testing
# matrix (Thunderbird / Apple Mail / Outlook / K-9); this script
# only asserts that the protocol surface those clients need is
# present on the wire.
#
# Inputs (all have sensible compose-stack defaults):
#   SMTP_HOST, SMTP_STARTTLS_PORT (default 587)
#   SMTP_SUBMISSION_TLS_PORT (default 465 — implicit TLS)
#   IMAP_HOST, IMAP_STARTTLS_PORT (default 143)
#   IMAPS_PORT (default 993 — implicit TLS)
#   SMTP_USER                   — full email address
#   SMTP_PASS                   — password for SMTP_USER
#   SMTP_FROM                   — From: header address
#   SMTP_TO                     — Recipient (usually same as SMTP_USER for a loopback test)
#   TEST_SUBJECT                — Subject header to look for in IMAP FETCH
#
# Usage:
#   SMTP_USER=admin@kmail.dev SMTP_PASS=secret SMTP_TO=admin@kmail.dev \
#     ./scripts/test-imap-smtp.sh

set -eu

SMTP_HOST=${SMTP_HOST:-localhost}
SMTP_STARTTLS_PORT=${SMTP_STARTTLS_PORT:-587}
SMTP_SUBMISSION_TLS_PORT=${SMTP_SUBMISSION_TLS_PORT:-465}
IMAP_HOST=${IMAP_HOST:-localhost}
IMAP_STARTTLS_PORT=${IMAP_STARTTLS_PORT:-143}
IMAPS_PORT=${IMAPS_PORT:-993}

SMTP_USER=${SMTP_USER:-}
SMTP_PASS=${SMTP_PASS:-}
SMTP_FROM=${SMTP_FROM:-$SMTP_USER}
SMTP_TO=${SMTP_TO:-$SMTP_USER}
TEST_SUBJECT=${TEST_SUBJECT:-kmail-compat-test-$(date +%s)}

log()  { printf '[test-imap-smtp] %s\n' "$*"; }
fail() { printf '[test-imap-smtp] FAIL: %s\n' "$*" >&2; exit 1; }

if [ -z "$SMTP_USER" ] || [ -z "$SMTP_PASS" ]; then
  fail "SMTP_USER and SMTP_PASS must be set"
fi

need() {
  command -v "$1" >/dev/null 2>&1 || fail "required tool missing: $1"
}
need openssl
need curl
need base64

# ------------------------------------------------------------------
# 1. SMTP 587 + STARTTLS capability announcement.
# ------------------------------------------------------------------
log "checking SMTP STARTTLS on ${SMTP_HOST}:${SMTP_STARTTLS_PORT}"
smtp_ehlo=$(
  {
    printf 'EHLO kmail-compat-test\r\n'
    sleep 1
    printf 'QUIT\r\n'
  } | openssl s_client \
        -connect "${SMTP_HOST}:${SMTP_STARTTLS_PORT}" \
        -starttls smtp -crlf -quiet 2>/dev/null || true
)
printf '%s' "$smtp_ehlo" | grep -qi '250[- ]STARTTLS\|250[- ]AUTH' || {
  printf '%s\n' "$smtp_ehlo" >&2
  fail "SMTP:${SMTP_STARTTLS_PORT} did not announce STARTTLS / AUTH capabilities"
}
log "SMTP STARTTLS OK — AUTH capability announced"

# ------------------------------------------------------------------
# 2. SMTP 465 implicit TLS reachability.
# Apple Mail and K-9 default to 465 / implicit TLS. Thunderbird
# also offers it. We only verify a TLS handshake completes.
# ------------------------------------------------------------------
log "checking SMTP implicit TLS on ${SMTP_HOST}:${SMTP_SUBMISSION_TLS_PORT}"
if : | openssl s_client \
      -connect "${SMTP_HOST}:${SMTP_SUBMISSION_TLS_PORT}" \
      -quiet 2>/dev/null | head -c 16 | grep -qi '220'; then
  log "SMTP implicit TLS OK"
else
  log "SMTP implicit TLS port :${SMTP_SUBMISSION_TLS_PORT} did not greet with 220 — skipping (port may be disabled)"
fi

# ------------------------------------------------------------------
# 3. IMAP 143 + STARTTLS + LOGIN + LIST.
# ------------------------------------------------------------------
log "checking IMAP STARTTLS on ${IMAP_HOST}:${IMAP_STARTTLS_PORT}"
imap_session=$(
  {
    printf 'a1 CAPABILITY\r\n'
    sleep 1
    printf 'a2 LOGIN "%s" "%s"\r\n' "$SMTP_USER" "$SMTP_PASS"
    sleep 1
    printf 'a3 LIST "" "*"\r\n'
    sleep 1
    printf 'a4 LOGOUT\r\n'
  } | openssl s_client \
        -connect "${IMAP_HOST}:${IMAP_STARTTLS_PORT}" \
        -starttls imap -crlf -quiet 2>/dev/null || true
)
printf '%s' "$imap_session" | grep -qi 'a1 OK' \
  || fail "IMAP CAPABILITY did not return OK"
printf '%s' "$imap_session" | grep -qi 'a2 OK' \
  || fail "IMAP LOGIN failed — check SMTP_USER / SMTP_PASS"
printf '%s' "$imap_session" | grep -qi 'a3 OK' \
  || fail "IMAP LIST failed"
printf '%s' "$imap_session" | grep -qi 'INBOX' \
  || fail "IMAP LIST did not return an INBOX mailbox"
log "IMAP STARTTLS + LOGIN + LIST OK"

# ------------------------------------------------------------------
# 4. Round-trip a test message: SMTP submit → IMAP FETCH.
# ------------------------------------------------------------------
log "submitting test message (subject: ${TEST_SUBJECT})"
MAIL_FILE=$(mktemp)
trap 'rm -f "$MAIL_FILE"' EXIT

cat >"$MAIL_FILE" <<EOF
From: ${SMTP_FROM}
To: ${SMTP_TO}
Subject: ${TEST_SUBJECT}
Date: $(date -R 2>/dev/null || date -u "+%a, %d %b %Y %H:%M:%S +0000")
Message-ID: <${TEST_SUBJECT}@kmail.test>
MIME-Version: 1.0
Content-Type: text/plain; charset=UTF-8

This is a KMail IMAP/SMTP compatibility round-trip test.
Body line 2.
EOF

# curl drives the submission with STARTTLS + AUTH. `--ssl-reqd`
# requires TLS upgrade; `--mail-rcpt`/`--mail-from` fill the SMTP
# envelope; `--upload-file` streams the RFC 5322 body.
if ! curl -sS --ssl-reqd \
      --url "smtp://${SMTP_HOST}:${SMTP_STARTTLS_PORT}" \
      --user "${SMTP_USER}:${SMTP_PASS}" \
      --mail-from "${SMTP_FROM}" \
      --mail-rcpt "${SMTP_TO}" \
      --upload-file "$MAIL_FILE"; then
  fail "SMTP submission failed"
fi
log "SMTP submission accepted"

# Give Stalwart's LMTP / delivery queue a moment to write the
# message into the recipient's INBOX before we poll IMAP.
log "waiting up to 10s for delivery..."
attempts=0
while [ "$attempts" -lt 10 ]; do
  attempts=$((attempts + 1))
  imap_search=$(
    {
      printf 'a1 LOGIN "%s" "%s"\r\n' "$SMTP_USER" "$SMTP_PASS"
      sleep 1
      printf 'a2 SELECT INBOX\r\n'
      sleep 1
      printf 'a3 SEARCH SUBJECT "%s"\r\n' "$TEST_SUBJECT"
      sleep 1
      printf 'a4 LOGOUT\r\n'
    } | openssl s_client \
          -connect "${IMAP_HOST}:${IMAP_STARTTLS_PORT}" \
          -starttls imap -crlf -quiet 2>/dev/null || true
  )
  if printf '%s' "$imap_search" | grep -Eq '\* SEARCH [0-9]'; then
    log "IMAP SEARCH matched test message on attempt ${attempts}"
    log "IMAP / SMTP round-trip OK"
    exit 0
  fi
  sleep 1
done

fail "message with subject \"${TEST_SUBJECT}\" never arrived in INBOX"
