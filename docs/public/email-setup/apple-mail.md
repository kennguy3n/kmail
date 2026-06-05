---
title: Set up KMail in Apple Mail
description: Add your KMail account to Apple Mail on macOS and iOS, including calendar and contacts.
category: Email Setup
order: 20
updated: 2026-06-05
---

Apple Mail on macOS and iOS connects to KMail over IMAP and SMTP, with
calendars and contacts via CalDAV/CardDAV.

## macOS

1. **Mail** → **Settings** → **Accounts** → **+** → **Other Mail
   Account…**
2. Enter your name, KMail address, and password, then **Sign In**.
3. If automatic setup doesn't complete, enter the servers manually:

| Setting   | Incoming (IMAP)        | Outgoing (SMTP)        |
| --------- | ---------------------- | ---------------------- |
| Host name | `imap.kmail.kchat.dev` | `smtp.kmail.kchat.dev` |
| Port      | `993` (SSL)            | `465` (SSL)            |
| Username  | full email address     | full email address     |

## iOS / iPadOS

1. **Settings** → **Mail** → **Accounts** → **Add Account** → **Other**.
2. **Add Mail Account**, enter your details, and tap **Next**.
3. Choose **IMAP** and confirm the incoming/outgoing servers above.

## Calendars & contacts

1. **Settings** → **Calendar** (or **Contacts**) → **Accounts** →
   **Add Account** → **Other** → **Add CalDAV/CardDAV Account**.
2. Server: `dav.kmail.kchat.dev`, with your email and password.

See [Calendar setup](/help/calendar/caldav-setup) for details.

## Two-factor accounts

If your account uses TOTP or a passkey, generate an app-specific
password in the web app under **Security settings** and use it in Apple
Mail.
