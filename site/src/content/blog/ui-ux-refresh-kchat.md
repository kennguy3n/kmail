---
title: "KMail UI/UX refresh: privacy email that feels like KChat"
description: "How we aligned KMail's web app, desktop client, and marketing site with the KChat umbrella brand — and what the refresh means for everyday users."
pubDate: 2026-06-24
author: The KMail team
tags: [product, design, kchat]
---

KMail is private business email and calendar for teams. From day one we built it on open standards (JMAP, IMAP, SMTP, CalDAV, CardDAV) and privacy-by-design architecture. But a product that protects your data still has to *feel* effortless. Over the last sprint we audited every screen, compared it to the best-in-class products we admire, and aligned KMail's visual language with the KChat umbrella.

The result is a cleaner, more consistent interface that still ships the same encryption, zero-access vaults, and compliance tooling underneath.

## What changed

### One brand palette across web, desktop, and site

KChat uses a friendly indigo brand (`#4f46e5`) with the Inter typeface, soft 12 px cards, and 8 px buttons. We brought that exact palette into the KMail web app and desktop client so the products feel like siblings, not cousins.

- Primary actions now use the same indigo accent as KChat.
- Inter is loaded across the web app, replacing the generic system-only stack.
- Border radius, shadows, and spacing were tuned to match KChat's softer, more approachable scale.

### Polished empty states

Empty inboxes, calendars, and vaults used to show plain text like "No messages." Now every key list view surfaces a friendly `EmptyState` component with an icon, a clear explanation, and a contextual action.

![KMail inbox empty state](/screenshots/01-mail-inbox.png)

The inbox invites you to compose; the calendar prompts you to create an event; contacts and labels explain why the list is empty and how to add the first item. Small touches, but they remove the "is this broken?" anxiety users feel in empty screens.

### A cleaner signup funnel

The self-service signup page was rebuilt on semantic tokens so it respects the user's light/dark preference and shares the same card, input, and button styling as the rest of the app. No more hardcoded blue or slate colors that drifted from the rest of the product.

![KMail signup flow](/screenshots/21-onboarding.png)

### Refreshed app chrome

The top bar logo now uses a rounded indigo envelope mark that mirrors the KChat favicon. The global search bar, account menu, and sidebar all share the same token-driven colors, so dark mode flips cleanly without extra overrides.

## Why it matters for SMEs

Small and mid-sized businesses don't have a dedicated IT design team. Every screen has to be self-explanatory. By aligning with KChat, we reduce the learning curve for anyone who already uses KChat for messaging: buttons, badges, and navigation behave the same way. For teams switching from Gmail or Microsoft 365, the polish signals that privacy doesn't have to mean primitive.

## What's next

The UI/UX refresh is the foundation. We are now working through the remaining audit items: skeleton loaders for heavy admin tables, inline validation across forms, and a more animated mobile drawer. If you have feedback on a specific screen, [email us](mailto:support@kmail.kchat.dev) or open a discussion.

[Explore the features](/features) or [start a workspace](/signup).
