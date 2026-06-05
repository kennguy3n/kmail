---
title: Migrate from Gmail / Google Workspace
description: Move mail, calendars, and contacts from Gmail or Google Workspace to KMail with the migration tool.
category: Migration
order: 10
updated: 2026-06-05
---

KMail's migration automation (Pro and Privacy plans) imports your
existing mail, calendars, and contacts from Gmail or Google Workspace
with minimal downtime.

## Before you start

- You're on the **Pro** or **Privacy** plan.
- Your domain is verified and [DNS is set up](/help/getting-started/dns-setup).
- For a whole organization, you have Google **Workspace admin** access;
  for a single mailbox, the account password or an app password.

## Recommended approach: cut over with dual delivery

1. **Provision users** in KMail (manually or via
   [SCIM](/help/admin/scim-provisioning)).
2. **Start the migration** under **Admin → Migrations** → *New job* →
   *Google*. Authenticate with OAuth (Workspace) or per-mailbox
   credentials.
3. KMail copies mail folders/labels, calendars, and contacts in the
   background. You can watch progress per mailbox
   (`GET /api/v1/migrations/{jobId}`).
4. **Switch MX** to KMail (see DNS setup). New mail now arrives in KMail.
5. **Run a delta sync** to catch anything that landed in Gmail during
   the switch.

## What migrates

| Data       | Notes                                                       |
| ---------- | ----------------------------------------------------------- |
| Mail       | Folders/labels are mapped to KMail mailboxes; read state preserved. |
| Calendars  | Events, recurrence, and attendees via CalDAV.               |
| Contacts   | Personal contacts via CardDAV.                              |

Chat, Drive files, and Google-specific features are out of scope.

## After migration

- Confirm a few mailboxes look right (folders, recent mail, calendar).
- Update clients to KMail ([Thunderbird](/help/email-setup/thunderbird),
  [Apple Mail](/help/email-setup/apple-mail)).
- Tighten [DMARC](/help/security/dmarc-explained) to `reject` once mail
  flows through KMail.
- Keep the Google account read-only for a couple of weeks as a safety
  net, then decommission.

## Troubleshooting

- **Rate limits:** very large mailboxes are throttled by Google; the job
  resumes automatically.
- **Missing items:** re-run a **delta sync** — it only copies what's new
  or changed since the last run.
