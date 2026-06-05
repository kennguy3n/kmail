---
title: Calendar & contacts (CalDAV / CardDAV)
description: Connect your calendars and address books to KMail over open CalDAV and CardDAV standards.
category: Calendar
order: 10
updated: 2026-06-05
---

KMail provides standards-based calendaring (CalDAV) and contacts
(CardDAV), so your schedule and address book sync with the web app and
any compatible client.

## Discovery

Clients that support service discovery can find your calendar
automatically from `GET /.well-known/caldav`. Otherwise use the server
`dav.kmail.kchat.dev` with your full email and password.

## What's included

- **Personal calendars** — create as many as you like.
- **Shared calendars** — delegate view or edit access to teammates.
- **Resource calendars** — book rooms or equipment with conflict checks.
- **Free/busy** — publish availability so others can find a time without
  seeing event details.
- **Contacts** — personal address books plus the tenant-wide Global
  Address List (GAL).

## Adding a calendar

- **Apple Calendar:** Settings → Accounts → Add CalDAV account →
  `dav.kmail.kchat.dev`.
- **Thunderbird:** New Calendar → On the Network → CalDAV → use the
  collection URL shown in the web app under Calendar settings.

## Free/busy lookup

When creating an event in the web app, use **Check availability** to see
attendees' free/busy windows. This uses RFC 5545 VFREEBUSY data and
never exposes private event details.

## Troubleshooting

- If a client only shows one calendar, point it at the **principal**
  collection URL (ends in `/`) rather than a single calendar.
- Make sure the account type is **CalDAV** (calendars) or **CardDAV**
  (contacts) — they are separate accounts in most clients.
