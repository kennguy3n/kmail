# KMail — Third-Party Client Compatibility

This document captures the Phase 2 compatibility contract for
third-party mail and calendar clients. KMail's primary UX is the
React pane inside KChat talking JMAP through the Go BFF — see
`docs/JMAP-CONTRACT.md`. IMAP, SMTP, CalDAV, CardDAV, and WebDAV
exist so users can continue to use Thunderbird, Apple Mail, K-9,
Apple Calendar, and similar clients when the KChat web surface is
not a fit (offline-heavy workflows, desktop-native integrations).

Do **not** treat the non-JMAP protocols as the primary UX — per
`note-kennguy3n-kmail` do-not-do list, "JMAP through the Go BFF
is the primary path."

- **Mail core**: Stalwart Mail Server v0.16.0 (pinned in
  `docker-compose.yml` and `docs/PROPOSAL.md`).
- **Transport security**: TLS 1.2+ required on every user-facing
  port; STARTTLS on :143 / :587 and implicit TLS on :465 / :993.
- **Auth**: SASL PLAIN over TLS; OAuth2 / XOAUTH2 is a Phase 4
  deliverable and not yet supported.

---

## Port matrix

| Protocol       | Host port | TLS mode       | Notes                                    |
| -------------- | --------- | -------------- | ---------------------------------------- |
| SMTP (MTA)     | 25        | STARTTLS       | Inbound-only; not used by MUA clients.   |
| SMTP submission| 587       | STARTTLS       | Primary submission port for MUAs.        |
| SMTP submission| 465       | Implicit TLS   | Preferred by Apple Mail / K-9.           |
| IMAP           | 143       | STARTTLS       | Primary mailbox read port for MUAs.      |
| IMAPS          | 993       | Implicit TLS   | Preferred by Apple Mail.                 |
| CalDAV / HTTP  | 8080      | (reverse-proxy)| Terminate TLS at the edge; see below.    |
| JMAP           | 8080      | (reverse-proxy)| Bearer auth; not for MUA clients.        |

In local development every port listens on `localhost` with the
self-signed dev cert Stalwart generates on first boot. In
staging/production TLS terminates at the edge load balancer and
the origin (Stalwart + the Go BFF) speaks plaintext inside the
mesh.

---

## Thunderbird

Tested against Thunderbird 115 ESR and 128 ESR.

**Incoming (IMAP):**
- Protocol: IMAP
- Server hostname: `<your.kmail.host>` (e.g. `localhost` in dev)
- Port: `143`
- Connection security: `STARTTLS`
- Authentication method: `Normal password`
- Username: full email address (e.g. `alice@kmail.dev`)

**Outgoing (SMTP):**
- Server hostname: same as above
- Port: `587`
- Connection security: `STARTTLS`
- Authentication method: `Normal password`
- Username: full email address

**Manual setup only:** Thunderbird's autoconfig wizard relies on
`https://autoconfig.<domain>/mail/config-v1.1.xml`. KMail will
serve this automatically once the DNS wizard lands autoconfig
records in Phase 3; until then, configure manually.

Phase 2 quick-checklist for Thunderbird:
- [ ] Connect to INBOX over :143 / STARTTLS.
- [ ] Send a message via :587 / STARTTLS to an internal address.
- [ ] Verify the message appears in INBOX within 10 s.
- [ ] Create a subfolder; verify it persists after reconnect.
- [ ] Open a message with a ≥1 MB attachment; verify FETCH succeeds.
- [ ] Flag / unflag / mark-read; verify state persists.
- [ ] Search inbox by subject; verify Stalwart's SEARCH returns
      the expected UID set.

---

## Apple Mail (macOS + iOS)

Tested against macOS Mail 16 (Sonoma / Sequoia) and iOS Mail 17.

**Incoming (IMAP):**
- Account type: `IMAP`
- Description: anything
- Incoming Mail Server: `<your.kmail.host>`
- Username: full email address
- Password: user password
- Advanced: Port `993`, `Use SSL` (implicit TLS) — Apple Mail
  treats 143/STARTTLS as a legacy fallback.

**Outgoing (SMTP):**
- Server: `<your.kmail.host>`
- Username: full email address
- Password: user password
- Use SSL: yes
- Authentication: `Password`
- Port: `465` (implicit TLS) or `587` (STARTTLS) — Apple Mail
  auto-discovers, but pinning 465 avoids a spurious STARTTLS
  negotiation failure some older macOS builds print on first
  send.

Phase 2 quick-checklist for Apple Mail:
- [ ] Add account; verify incoming + outgoing probes succeed.
- [ ] Pull INBOX; verify unread counts match JMAP client.
- [ ] Send a message with an image attachment; verify delivery.
- [ ] Move a message between mailboxes; verify sync with JMAP
      client.
- [ ] Create a rule that files messages from a known sender; verify
      the rule fires (client-side, but proves IMAP IDLE is live).
- [ ] Toggle "Use Secure Authentication"; verify AUTH PLAIN
      completes without fallback warnings.

---

## Known limitations (Stalwart v0.16.0)

- **XOAUTH2 / OAuth2 over IMAP/SMTP** is not supported in Phase 2.
  Clients must use `AUTH PLAIN` / `AUTH LOGIN` over TLS. OAuth2
  ships in Phase 4 alongside the KChat OIDC integration.
- **IMAP CONDSTORE / QRESYNC** are enabled but not yet performance-
  tuned at scale. Heavy offline sync may trip Stalwart's default
  command timeout on very large mailboxes (>100k messages); split
  the account across multiple mailboxes if this happens.
- **IMAP SORT / THREAD** return server-side results but do not yet
  honour per-locale collation. Apple Mail reorders client-side so
  this is rarely user-visible.
- **SMTP BURL / CHUNKING** are disabled. Thunderbird falls back to
  standard `DATA` automatically.
- **DSN (delivery status notifications)** only fire for
  `NOTIFY=FAILURE` — `DELAYED` and `SUCCESS` are not wired through
  to the tenant.
- **Push IMAP NOTIFY** is disabled; clients poll with IMAP IDLE
  instead. This is a Stalwart v0.16.0 default we keep for Phase 2
  stability.
- **Exchange / ActiveSync / MAPI** is explicitly out of scope
  through Phase 4 (see `note-8c8a33156d774657b2fe4be485a103d0`).

---

## Manual test checklist

Run this list against every client we add to the support matrix.
Use `./scripts/test-imap-smtp.sh` for the automated subset.

Mail:
- [ ] Account setup (incoming + outgoing) completes without
      errors.
- [ ] INBOX opens; unread counts match the JMAP client.
- [ ] Send a plain-text message to an internal address; delivery
      lands within 10 s.
- [ ] Send a plain-text message to an external address; verify
      Stalwart queues with TLS out and the message arrives.
- [ ] Send a message with a ≥1 MB attachment; verify it downloads
      intact.
- [ ] Receive a message with a ≥5 MB attachment; verify FETCH /
      preview succeeds.
- [ ] Mark read, flag, move, delete; verify each operation
      persists server-side (check via JMAP client or IMAP
      FETCH FLAGS).
- [ ] Create a subfolder; move a message into it; verify the
      folder appears for a second client on the same account.
- [ ] Empty trash; verify deletes propagate.
- [ ] Search by subject, sender, body keyword; verify each returns
      the expected hits (Stalwart uses Meilisearch under the hood
      for full-text search).
- [ ] Disconnect the network; reconnect; verify IDLE re-establishes
      without a manual refresh.

Spam / junk (new in Phase 2 — see `scripts/stalwart-init.sh`):
- [ ] Send a test message with `X-Spam-Status: Yes` via SMTP;
      verify it lands in the Junk mailbox (not INBOX).
- [ ] Mark a message in INBOX as spam from the client; verify it
      moves to Junk and the `$junk` IMAP keyword is set.
- [ ] Mark a message in Junk as not spam; verify it moves back to
      INBOX and the `$junk` keyword clears / `$notjunk` sets.

---

## Running the automated IMAP/SMTP test

```sh
SMTP_USER=admin@kmail.dev \
SMTP_PASS=<password> \
SMTP_TO=admin@kmail.dev \
./scripts/test-imap-smtp.sh
```

What it checks:
1. SMTP :587 announces `STARTTLS` + `AUTH` capabilities.
2. SMTP :465 (implicit TLS) completes a TLS handshake.
3. IMAP :143 accepts STARTTLS, `LOGIN`, and `LIST "" "*"` and
   returns an `INBOX` mailbox.
4. A round-trip: curl submits an RFC 5322 message via SMTP :587;
   the script polls IMAP for up to 10 s until the same subject
   shows up in the recipient's INBOX.

The script exits non-zero on the first check that fails and
prints the relevant protocol dialogue.

---

## CalDAV

See `scripts/test-caldav.sh` for the automated smoke test.

Apple Calendar and Thunderbird (with the Lightning /
`TbSync+CalDAV` integration) both drive CalDAV over HTTP(S) to
Stalwart's `/dav/calendars/` endpoint. In local development the
endpoint is published on `http://localhost:8080/dav/calendars/`
and served without TLS; stage the Go BFF in front of it in
production so TLS terminates at the edge.

**Apple Calendar (macOS + iOS):**
- Account type: `CalDAV`
- Account Type (advanced): `Manual`
- Username: full email address
- Password: user password
- Server address: `<your.kmail.host>`
- Server path: `/dav/calendars/<account-id>/`
- Port: `443` in production (`8080` in local dev, no TLS)
- Use SSL: yes in production

**Thunderbird (Lightning or TbSync + CalDAV):**
- New Calendar → On the Network → Location:
  `http<s>://<your.kmail.host>/dav/calendars/<account-id>/<calendar-name>/`
- Authentication: username / password when prompted.

KMail also exposes the draft JMAP calendars capability
(`urn:ietf:params:jmap:calendars`) through the Go BFF — the
React Calendar pane uses that path exclusively. CalDAV exists for
third-party clients only, so the two surfaces must stay in sync
against the same underlying Stalwart CalDAV store.

Phase 2 quick-checklist for CalDAV clients:
- [ ] Account autodetects `calendar-home-set` via PROPFIND.
- [ ] Primary calendar appears; events listed in the month view
      match the JMAP client.
- [ ] Create an event from the client; verify it appears in the
      React Calendar pane within 5 s.
- [ ] Edit the event (title, start/end, location); verify both
      surfaces reflect the change.
- [ ] Delete the event from one surface; verify it disappears
      from the other.
- [ ] Create a recurring event (FREQ=WEEKLY); verify instances
      render in both surfaces.
- [ ] Accept / decline an invite from the client; verify RSVP
      propagates.

## Known limitations (CalDAV)

- **CardDAV contacts** ship with Stalwart but are not yet surfaced
  in KMail's docs — Phase 3 deliverable.
- **Scheduling (iMIP / iTIP) across domains** is not exercised
  automatically by these tests. Invites between tenants on the
  same Stalwart instance work; external invites depend on
  deliverability infrastructure that only reaches parity in
  Phase 4.
- **Calendar sharing / delegated access** is a Phase 4 feature.
  Team calendars via shared inboxes are NOT the same surface.
- **Free/busy lookups** over `REPORT` are implemented but not yet
  integrated with the KChat scheduling bridge.

## Running the automated CalDAV test

```sh
CALDAV_USER=admin@kmail.dev \
CALDAV_PASS=<password> \
./scripts/test-caldav.sh
```

What it checks:
1. `OPTIONS` against `/dav/calendars/` announces the CalDAV
   compliance class (`DAV: 1, 2, 3, calendar-access`).
2. `PROPFIND` with `Depth: 0` returns a multistatus response
   describing the user's calendar home.
3. `PROPFIND` with `Depth: 1` lists at least one calendar
   collection.
4. `PUT` writes a minimal VEVENT; the script re-reads it via
   `GET` and verifies the `SUMMARY` round-tripped intact.
5. `DELETE` removes the test event cleanly (204).
