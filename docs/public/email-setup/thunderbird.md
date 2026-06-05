---
title: Set up KMail in Thunderbird
description: Connect Mozilla Thunderbird to KMail using automatic configuration or manual IMAP/SMTP settings.
category: Email Setup
order: 10
updated: 2026-06-05
---

Mozilla Thunderbird works with KMail over IMAP and SMTP, and can
configure itself automatically if your domain has the autoconfig record
from the [DNS setup guide](/help/getting-started/dns-setup).

## Automatic setup (recommended)

1. Open Thunderbird → **Account Settings** → **Account Actions** →
   **Add Mail Account**.
2. Enter your name, your full KMail address, and your password.
3. Click **Continue**. Thunderbird reads your domain's autoconfig
   record and fills in the servers for you.
4. Click **Done**.

## Manual setup

If autoconfig isn't available, enter these settings:

| Setting        | Incoming (IMAP)            | Outgoing (SMTP)            |
| -------------- | -------------------------- | -------------------------- |
| Server         | `imap.kmail.kchat.dev`     | `smtp.kmail.kchat.dev`     |
| Port           | `993`                      | `465`                      |
| Security       | SSL/TLS                    | SSL/TLS                    |
| Authentication | Normal password / OAuth2   | Normal password / OAuth2   |
| Username       | your full email address    | your full email address    |

## Calendars and contacts

Thunderbird supports CalDAV and CardDAV. See
[Calendar setup](/help/calendar/caldav-setup) to add your KMail calendar
and address book.

## Troubleshooting

- **"Configuration could not be verified"** — double-check the port and
  that security is set to SSL/TLS (not STARTTLS) for ports 993/465.
- **Authentication fails** — if you have two-factor enabled, sign in
  with OAuth2 or generate an app password from the web app.
