---
title: IMAP, SMTP & CalDAV server settings
description: The complete reference of KMail server hostnames, ports, and security modes for any standards-compliant mail, calendar, or contacts client.
category: Email Setup
order: 5
updated: 2026-06-06
---

KMail speaks open standards, so any compliant client works: Thunderbird,
Apple Mail, Outlook, K-9 Mail, Evolution, mutt, and more. Most clients
configure themselves automatically from your domain's autodiscovery
records (see the [DNS setup guide](/help/getting-started/dns-setup)).
When you need to enter settings by hand, use the tables below.

> Replace `kmail.kchat.dev` with your own mail domain if you run a
> custom domain. The hostnames follow the `imap.`, `smtp.`, and `dav.`
> prefixes published by autodiscovery.

## Incoming mail (IMAP)

| Setting        | Value                                  |
| -------------- | -------------------------------------- |
| Server         | `imap.kmail.kchat.dev`                 |
| Port (implicit TLS) | `993` (recommended)               |
| Port (STARTTLS)     | `143`                             |
| Username       | your full email address                |
| Authentication | Normal password or OAuth2              |

Prefer port `993` with implicit TLS. Port `143` is offered with
STARTTLS for clients that default to it; KMail requires TLS on both —
plaintext IMAP is never accepted.

## Outgoing mail (SMTP submission)

| Setting        | Value                                  |
| -------------- | -------------------------------------- |
| Server         | `smtp.kmail.kchat.dev`                 |
| Port (implicit TLS) | `465` (recommended)               |
| Port (STARTTLS)     | `587`                             |
| Username       | your full email address                |
| Authentication | Normal password or OAuth2              |

Use the submission ports (`465` or `587`) for sending. Port `25` is for
server-to-server delivery (inbound MX) only and is not used by mail
clients.

## Calendars & contacts (CalDAV / CardDAV)

| Setting   | Value                                       |
| --------- | ------------------------------------------- |
| Server    | `dav.kmail.kchat.dev`                       |
| Port      | `443` (HTTPS)                               |
| Username  | your full email address                     |

Most clients auto-discover the principal and calendar/address-book
collections once you enter the server and credentials. See the
[CalDAV setup guide](/help/calendar/caldav-setup) for step-by-step
instructions.

## Transport security

- TLS 1.2 or newer is required on every user-facing port.
- Implicit TLS (`993` / `465`) is preferred; STARTTLS (`143` / `587`)
  is supported for clients that default to it.
- KMail does not accept unencrypted connections on any port.

## Two-factor accounts

If your account uses TOTP or a passkey, clients that don't support
OAuth2 (older IMAP/SMTP clients) need an **app password**. Generate one
in the web app under **Security settings** and use it in place of your
login password.

## Client-specific guides

- [Thunderbird](/help/email-setup/thunderbird)
- [Apple Mail](/help/email-setup/apple-mail)
- [Calendar (CalDAV)](/help/calendar/caldav-setup)

## Troubleshooting

- **"Cannot verify server identity" / TLS errors** — confirm the port
  matches the security mode (implicit TLS on `993`/`465`, STARTTLS on
  `143`/`587`).
- **Authentication fails** — with two-factor enabled, use OAuth2 or an
  app password.
- **Delivery problems after setup** — see
  [Delivery issues](/help/troubleshooting/delivery-issues).
