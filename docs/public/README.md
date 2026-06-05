# KMail public help content

This directory holds the **canonical, user-facing help-center
articles** for KMail. They are plain Markdown with YAML frontmatter
so they live in the repo, are reviewable in pull requests, and ship
with the product.

The public marketing site (`site/`) renders these articles under
`/help`. At build time `site/scripts/sync-content.mjs` mirrors this
directory into `site/src/content/help/` (a gitignored mirror) where
Astro's content collection picks them up. **Edit articles here, not
in the mirror.**

## Frontmatter

Every article requires:

```yaml
---
title: DNS setup guide
description: One-line summary used in listings and meta tags.
category: Getting Started
order: 10          # sort order within the category (lower = first)
updated: 2026-06-05
---
```

## Layout

Each top-level folder is a help **category**; the file path becomes
the article URL, e.g. `getting-started/dns-setup.md` →
`/help/getting-started/dns-setup`.

Categories: Getting Started, Email Setup, Calendar, Admin, Security,
Billing, Migration, Troubleshooting.
