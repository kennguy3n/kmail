---
title: Shared inbox guide
description: Set up and manage shared inboxes like support@ or sales@ so your team can collaborate without extra paid seats.
category: Admin
order: 10
updated: 2026-06-05
---

A shared inbox is an address like `support@yourdomain.com` that several
teammates monitor together. KMail shared inboxes don't consume a paid
seat, and they include assignment, internal notes, and (on the Privacy
plan) MLS-encrypted group access.

## Create a shared inbox

1. Go to **Admin → Shared inboxes**.
2. Click **New shared inbox** and choose the address.
3. Add members. Each member can read, reply, assign, and note.

## Working in a shared inbox

- **Assign** a conversation to a teammate so it's clear who owns it.
- Add **internal notes** that recipients never see.
- Replies are sent **as the shared address**, not your personal one.

## Encryption (Privacy plan)

On the Privacy plan, shared inboxes use an MLS group so message keys are
shared only with current members. When you add or remove a member,
KMail rotates the group key automatically. You can check rotation status
in the inbox's **Security** tab.

## Permissions

Shared-inbox membership is managed by tenant admins. Removing a member
revokes their access immediately and (on Privacy) triggers a key
rotation so they can't read mail received after removal.

## Tips

- Use shared inboxes for role addresses (`billing@`, `careers@`) instead
  of forwarding rules, so history stays in one place.
- Combine with [Sieve rules](/help/admin/shared-inboxes) and labels to
  triage automatically.
