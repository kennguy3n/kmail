---
title: Plans & billing
description: How KMail plans, seats, and billing work — including upgrades, proration, invoices, and cancellation.
category: Billing
order: 10
updated: 2026-06-05
---

KMail is billed **per seat, per month** through Stripe. There are three
plans:

| Plan    | Price / seat / mo | Storage / seat | Daily send |
| ------- | ----------------- | -------------- | ---------- |
| Core    | $3                | 5 GB           | 500        |
| Pro     | $6                | 15 GB          | 2,000      |
| Privacy | $9                | 50 GB          | 5,000      |

See the full [pricing comparison](/pricing) for feature differences.

## Seats

A seat is a user mailbox. **Shared inboxes don't consume a paid seat**,
so you can run `support@`, `sales@`, and similar role addresses without
extra cost.

## Upgrades, downgrades & proration

You can change plans at any time under **Admin → Billing**. When you
upgrade mid-cycle, KMail shows a **proration preview** so you see the
exact prorated charge before confirming
(`GET /api/v1/tenants/{id}/billing/proration-preview`). Downgrades take
effect at the next renewal and respect the new plan's quotas.

## Invoices & usage

- **Current plan & usage:** `GET /api/v1/tenants/{id}/billing` and
  `/billing/usage`.
- **History:** `GET /api/v1/tenants/{id}/billing/history`.

Invoices and receipts are issued by Stripe and emailed to your billing
contact.

## Cancellation & data

You can cancel anytime; service continues until the end of the paid
period. Before or after cancellation you can **export** all mail,
calendars, and contacts (**Admin → Export**). After the cancellation
grace period, data is cryptographically deleted.

## Payment issues

If a payment fails, Stripe retries automatically and KMail keeps your
account active during the dunning window. Update your card under
**Admin → Billing** to resolve a past-due invoice immediately.
