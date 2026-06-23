---
title: "How KMail shares a design system with KChat"
description: "A technical look at the semantic tokens, Tailwind mapping, and theming decisions that let the web app, desktop client, and marketing site stay in sync."
pubDate: 2026-06-21
author: The KMail team
tags: [engineering, design-system, kchat]
---

KMail ships three surfaces: a React web app, an Electron desktop client, and a static Astro marketing site. Keeping them visually consistent is hard because they use different build tools and rendering environments. We solved it with a small layer of shared primitives and a strict rule: components read semantic tokens, never raw hex values.

## Semantic tokens in CSS

The web app uses CSS custom properties split into two layers:

- **Primitives** live in `web/src/styles/tokens.css` — the indigo palette, gray scale, spacing scale, radius, shadows, and typography.
- **Semantic tokens** live in `web/src/styles/themes/light.css` and `dark.css` — `color-primary`, `color-surface`, `color-text`, etc.

This means the same Tailwind classes (`bg-primary`, `text-fg-muted`, `shadow-md`) resolve to different values when `data-theme="dark"` is set on `<html>`. No per-component `dark:` overrides needed.

## Tailwind mapping

`tailwind.config.ts` maps the CSS variables onto Tailwind namespaces:

```ts
colors: {
  primary: {
    DEFAULT: "var(--color-primary)",
    hover: "var(--color-primary-hover)",
    active: "var(--color-primary-active)",
    fg: "var(--color-primary-text)",
    subtle: "var(--color-primary-subtle)",
  },
  // ...
}
```

Components import `cn` from `lib/cn` and compose these utilities. The result is a single source of truth for color, radius, and shadow while still allowing Tailwind's rapid layout utilities.

## Inter font everywhere

KChat uses Inter. We added the Google Fonts import to `web/index.html` and updated the `--font-sans` stack in tokens.css so the web app and desktop client both use it. The iframe that renders HTML message bodies also references Inter so external mail looks native without leaking styles.

## Desktop alignment

The desktop client is intentionally thin: it is a browser shell around the same React components. Its own `app.css` hardcodes a few legacy hex values for the shell chrome. We updated those to the same indigo and dark-slate palette so the window frame, sidebar, and buttons match the web app.

## Hardcoded colors removed

The biggest drift was in `Signup.tsx`, where the wizard had hardcoded blue (`#2563eb`) and slate colors that ignored the theme. We replaced those with semantic tokens (`bg-primary`, `bg-surface`, `text-fg-muted`, etc.). The signup funnel now inherits the KChat palette and dark-mode support automatically.

## Marketing site

The Astro site already used the KChat palette (`--c-brand: #4f46e5`). The refresh was mostly a matter of ensuring the web app matched it, so screenshots embedded in the site render as a continuous visual experience rather than a jarring product-vs-marketing contrast.

## Next steps

We are continuing the alignment by:

- Adding skeleton loaders to the remaining admin tables.
- Replacing raw form inputs with the shared `Input` component for inline validation.
- Auditing icon-only buttons for accessible labels.

If you are building a multi-surface product under a brand umbrella, the lesson is simple: invest in semantic tokens early, load the brand typeface in every shell, and make raw hex values a lint error.

[Read the API reference](/docs/api) or [browse the source on the features page](/features).
