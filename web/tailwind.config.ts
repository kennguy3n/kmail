import type { Config } from "tailwindcss";

/**
 * KMail Tailwind configuration.
 *
 * The single source of truth for *values* remains the CSS custom
 * properties in `src/styles/tokens.css` + `themes/{light,dark}.css`.
 * Here we map those semantic role tokens onto Tailwind's `theme`
 * namespaces so utilities (`bg-surface`, `text-fg-muted`,
 * `border-border`, `shadow-md`, …) resolve to the live CSS variables
 * and therefore flip automatically when `data-theme` changes — no
 * `dark:` variant required for the colour system itself.
 *
 * Loaded from `styles/global.css` via the `@config` directive so the
 * `@tailwindcss/vite` plugin picks it up.
 */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  // NOTE: the `dark:` variant is defined once, in `styles/global.css`
  // via `@custom-variant dark (...)` (Tailwind v4's CSS-first API).
  // We intentionally do NOT also set the v3-style `darkMode` option
  // here — defining it in both places is redundant and can emit
  // double/duplicated `dark:` selectors. The colour system auto-flips
  // through the semantic tokens, so `dark:` is rarely needed anyway.
  theme: {
    extend: {
      colors: {
        // Surfaces
        canvas: "var(--color-bg)",
        elevated: "var(--color-bg-elevated)",
        surface: {
          DEFAULT: "var(--color-surface)",
          muted: "var(--color-surface-muted)",
          hover: "var(--color-surface-hover)",
          active: "var(--color-surface-active)",
        },
        // Borders
        border: {
          DEFAULT: "var(--color-border)",
          strong: "var(--color-border-strong)",
        },
        // Text / foreground
        fg: {
          DEFAULT: "var(--color-text)",
          muted: "var(--color-text-muted)",
          subtle: "var(--color-text-subtle)",
          inverse: "var(--color-text-inverse)",
        },
        // Brand / accent
        primary: {
          DEFAULT: "var(--color-primary)",
          hover: "var(--color-primary-hover)",
          active: "var(--color-primary-active)",
          fg: "var(--color-primary-text)",
          subtle: "var(--color-primary-subtle)",
        },
        "on-accent": "var(--color-on-accent)",
        ring: "var(--color-focus-ring)",
        // Status
        success: {
          DEFAULT: "var(--color-success)",
          bg: "var(--color-success-bg)",
          fg: "var(--color-success-text)",
        },
        warning: {
          DEFAULT: "var(--color-warning)",
          bg: "var(--color-warning-bg)",
          fg: "var(--color-warning-text)",
        },
        danger: {
          DEFAULT: "var(--color-danger)",
          hover: "var(--color-danger-hover)",
          bg: "var(--color-danger-bg)",
          fg: "var(--color-danger-text)",
        },
        info: {
          DEFAULT: "var(--color-info)",
          bg: "var(--color-info-bg)",
          fg: "var(--color-info-text)",
        },
        overlay: "var(--color-overlay)",
      },
      boxShadow: {
        sm: "var(--shadow-sm)",
        md: "var(--shadow-md)",
        lg: "var(--shadow-lg)",
      },
      fontFamily: {
        sans: "var(--font-sans)",
        mono: "var(--font-mono)",
      },
      borderRadius: {
        sm: "var(--radius-sm)",
        md: "var(--radius-md)",
        lg: "var(--radius-lg)",
        xl: "var(--radius-xl)",
        "2xl": "var(--radius-2xl)",
        pill: "var(--radius-pill)",
      },
      zIndex: {
        sticky: "100",
        header: "200",
        drawer: "300",
        dropdown: "400",
        modal: "500",
        toast: "600",
        tooltip: "700",
      },
      maxWidth: {
        sidebar: "var(--sidebar-width)",
      },
      keyframes: {
        "fade-in": {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        "scale-in": {
          from: { opacity: "0", transform: "translateY(8px) scale(0.98)" },
          to: { opacity: "1", transform: "translateY(0) scale(1)" },
        },
        "slide-in-right": {
          from: { opacity: "0", transform: "translateX(16px)" },
          to: { opacity: "1", transform: "translateX(0)" },
        },
        shimmer: {
          "100%": { transform: "translateX(100%)" },
        },
      },
      animation: {
        "fade-in": "fade-in 120ms ease",
        "scale-in": "scale-in 150ms ease",
        "slide-in-right": "slide-in-right 180ms ease",
      },
    },
  },
} satisfies Config;
