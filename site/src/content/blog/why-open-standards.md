---
title: "Why we bet on open standards (JMAP, IMAP, CalDAV)"
description: "Open protocols are the antidote to email lock-in. Here's how KMail uses them."
pubDate: 2026-06-03
author: The KMail team
tags: [engineering, standards]
---

The fastest way to trap customers is a proprietary protocol. The fastest
way to earn their trust is to make leaving easy. KMail is built on open
standards on purpose.

## The protocols

- **JMAP** (RFC 8620/8621) powers our web and mobile clients with
  efficient, modern sync.
- **IMAP & SMTP** let you use Thunderbird, Apple Mail, or any classic
  client.
- **CalDAV & CardDAV** keep calendars and contacts portable.

## What that means for you

- **No lock-in.** Your data is reachable with standard clients and
  exportable at any time.
- **Choice of client.** Prefer Apple Mail? Thunderbird? Our web app?
  They all work.
- **Interoperability.** Standards-based calendaring means free/busy and
  invitations work across organizations.

Under the hood, KMail proxies a hardened Stalwart mail core through a
multi-tenant backend that adds billing, encryption, and compliance —
without hiding the open protocols underneath.

[Read the API reference](/docs/api) to build on top of KMail.
