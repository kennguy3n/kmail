---
title: Migrate from Microsoft 365
description: Move mailboxes, calendars, and contacts from Microsoft 365 / Exchange Online to KMail.
category: Migration
order: 20
updated: 2026-06-05
---

KMail's migration automation (Pro and Privacy plans) imports mail,
calendars, and contacts from Microsoft 365 / Exchange Online.

## Before you start

- You're on the **Pro** or **Privacy** plan.
- Your domain is verified and [DNS is set up](/help/getting-started/dns-setup).
- You have **Microsoft 365 admin** access (to consent to the migration
  app) or per-mailbox credentials/app passwords.

## Steps

1. **Provision users** in KMail (manually or via
   [SCIM](/help/admin/scim-provisioning)).
2. **Start the migration** under **Admin → Migrations** → *New job* →
   *Microsoft 365*. Grant the requested Graph/IMAP consent.
3. KMail copies mail folders, calendars, and contacts in the background;
   monitor per-mailbox progress (`GET /api/v1/migrations/{jobId}`).
4. **Switch MX** to KMail. New mail now arrives in KMail.
5. **Delta sync** to copy anything that arrived during the switch.

## What migrates

| Data       | Notes                                                      |
| ---------- | ---------------------------------------------------------- |
| Mail       | Folder hierarchy preserved; read/unread state retained.    |
| Calendars  | Events, recurrence, attendees.                             |
| Contacts   | Personal contacts; org directory via the GAL.              |

Teams, SharePoint, and OneDrive content are out of scope.

## After migration

- Spot-check mailboxes and calendars.
- Reconfigure Outlook/Apple Mail/Thunderbird to KMail. Outlook can use
  autodiscover if you published the record in DNS setup.
- Move [DMARC](/help/security/dmarc-explained) to `reject` once mail is
  flowing through KMail.

## Troubleshooting

- **Throttling:** Microsoft Graph/IMAP throttles large transfers; the
  job backs off and resumes.
- **Modern auth / MFA:** prefer admin consent (OAuth) over basic auth,
  which Microsoft has largely disabled. For single mailboxes use an app
  password.
- **Shared mailboxes:** import these as KMail
  [shared inboxes](/help/admin/shared-inboxes).
