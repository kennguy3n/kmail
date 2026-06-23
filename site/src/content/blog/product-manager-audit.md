---
title: "A product manager's view of the KMail UI/UX audit"
description: "What we learned from auditing every KMail screen against top-tier email and collaboration products, and how we prioritized the fixes."
pubDate: 2026-06-19
author: The KMail team
tags: [product, ux, audit]
---

We recently audited every KMail screen against top-tier email and collaboration products. The goal was simple: make KMail feel as polished as the tools SMEs already use, while keeping the privacy promise intact. This post is for product managers who want to understand the audit method, the findings, and the roadmap.

## Audit method

We reviewed:

- The 28 key UI flows captured in `docs/screenshots/` (inbox, compose, calendar, admin, security, billing, etc.).
- Every shared component in `web/src/components/ui/`.
- The public site (`site/`) and the desktop client (`apps/desktop/`).
- Competitive benchmarks: Gmail, Outlook, ProtonMail, Fastmail, Notion, Linear.

We scored issues by impact on user confidence and effort to fix. High-impact, low-effort fixes went first.

## Top findings

### 1. Brand consistency across surfaces

The web app used a blue primary color while the site and KChat used indigo. The desktop client used yet another set of hardcoded values. We unified all three to a single token-driven palette rooted in KChat's indigo and Inter font.

### 2. Empty states were under-designed

Plain text like "No messages" or "No calendars" does not tell the user what to do next. We replaced these with a shared `EmptyState` component that includes an icon, a sentence of context, and a call-to-action.

### 3. Loading and error feedback needed polish

Many pages showed static text while data loaded. We are now introducing skeleton screens and the existing `Toast` provider for transient errors. The inbox already uses the new empty-state and loading patterns.

### 4. The public signup flow felt isolated

The signup page looked different from the authenticated app because it used hardcoded colors. It now uses the same semantic tokens and respects the user's theme preference.

### 5. Admin tables needed visual hierarchy

Admin pages are information-dense. We are adding card grouping, section headers, and better spacing to make scan-and-act faster.

## What we shipped first

The first sprint delivered:

- KChat-aligned design tokens (indigo, Inter, radius, shadows).
- `EmptyState` component deployed across inbox, calendar, contacts, labels, templates, signatures, vault, and protected folders.
- Signup page rebuilt on semantic tokens.
- Desktop client reskinned to the same palette.
- 28 refreshed screenshots for the showcase series.

## What's on the roadmap

Next sprints will address:

- Skeleton loaders for the remaining admin tables.
- Inline form validation using the shared `Input` component.
- Animated mobile drawer with backdrop.
- Better confidential-send visual feedback in compose.
- Calendar color-coding and RSVP status badges.
- Search and filtering in the contacts view.

## How we measure success

We will track:

- Visual regression baselines in Playwright.
- User-reported confusion during onboarding.
- Time-to-first-email for new tenants.
- Support tickets related to empty states and loading.

## For PMs building privacy products

The lesson from this audit is that privacy features do not excuse rough UX. Users judge trust partly by polish. If the interface feels fragile, they assume the security is fragile too. Investing in a shared design system, friendly empty states, and consistent theming is not vanity work — it is part of the privacy promise.

[See the refreshed screenshots](/features) or [start a workspace](/signup).
