/**
 * `useMediaQuery` — subscribe to a CSS media query and re-render
 * when it flips. Used by the Layout to decide when the sidebar
 * should collapse into a drawer / bottom tab bar.
 *
 * The breakpoint pixel values mirror `--bp-*` in `styles/tokens.css`.
 * Keep the two in sync: CSS `@media` cannot read custom properties,
 * so the literals necessarily live in both places.
 */
import { useSyncExternalStore } from "react";

export const BREAKPOINTS = {
  sm: 640,
  md: 900,
  lg: 1200,
} as const;

export function useMediaQuery(query: string): boolean {
  function subscribe(callback: () => void): () => void {
    if (
      typeof window === "undefined" ||
      typeof window.matchMedia !== "function"
    ) {
      return () => {};
    }
    const mql = window.matchMedia(query);
    mql.addEventListener("change", callback);
    return () => mql.removeEventListener("change", callback);
  }

  function getSnapshot(): boolean {
    if (
      typeof window === "undefined" ||
      typeof window.matchMedia !== "function"
    ) {
      return false;
    }
    return window.matchMedia(query).matches;
  }

  // Server snapshot: assume desktop (no match for "max-width" mobile
  // queries) so SSR/test render is the wide layout by default.
  return useSyncExternalStore(subscribe, getSnapshot, () => false);
}

/** Convenience: true when the viewport is at or below the `md`
 *  breakpoint (the point at which the sidebar becomes a drawer). */
export function useIsMobile(): boolean {
  return useMediaQuery(`(max-width: ${BREAKPOINTS.md - 1}px)`);
}
