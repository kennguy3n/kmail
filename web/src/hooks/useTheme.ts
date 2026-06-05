/**
 * Theme management for KMail.
 *
 * Exposes a {@link useTheme} hook backed by a tiny module-level
 * store (via `useSyncExternalStore`) so every consumer — the
 * header toggle, a settings page, anything — observes one source
 * of truth and stays in sync without prop-drilling or a context
 * provider.
 *
 * Preference model:
 *   - `"light"` / `"dark"` — an explicit user choice, persisted to
 *     localStorage.
 *   - `"system"` — follow the OS `prefers-color-scheme` and react
 *     to changes live. This is the default when nothing is stored.
 *
 * The *resolved* theme (always `"light"` or `"dark"`) is written to
 * `document.documentElement[data-theme]`; all CSS custom properties
 * key off that attribute.
 */
import { useCallback, useSyncExternalStore } from "react";

export type ThemePreference = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

export const THEME_STORAGE_KEY = "kmail-theme";

const VALID_PREFERENCES: readonly ThemePreference[] = [
  "light",
  "dark",
  "system",
];

function isThemePreference(value: unknown): value is ThemePreference {
  return (
    typeof value === "string" &&
    (VALID_PREFERENCES as readonly string[]).includes(value)
  );
}

function prefersDark(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-color-scheme: dark)").matches
  );
}

function readStoredPreference(): ThemePreference {
  if (typeof window === "undefined") return "system";
  let stored: string | null = null;
  try {
    stored = window.localStorage.getItem(THEME_STORAGE_KEY);
  } catch {
    // localStorage can throw in private-mode / sandboxed iframes —
    // fall back to system rather than crashing the app.
    stored = null;
  }
  return isThemePreference(stored) ? stored : "system";
}

function resolve(preference: ThemePreference): ResolvedTheme {
  if (preference === "system") return prefersDark() ? "dark" : "light";
  return preference;
}

/** Apply the resolved theme to <html> so CSS variables switch. */
function applyResolvedTheme(resolved: ResolvedTheme): void {
  if (typeof document === "undefined") return;
  document.documentElement.setAttribute("data-theme", resolved);
}

// ---- Module-level store -------------------------------------------------

let preference: ThemePreference = readStoredPreference();
const listeners = new Set<() => void>();

function emit(): void {
  for (const listener of listeners) listener();
}

function setPreference(next: ThemePreference): void {
  preference = next;
  try {
    if (typeof window !== "undefined") {
      window.localStorage.setItem(THEME_STORAGE_KEY, next);
    }
  } catch {
    // Persisting is best-effort; ignore quota / access errors.
  }
  applyResolvedTheme(resolve(next));
  emit();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);

  // While in "system" mode, react to live OS theme changes.
  let mql: MediaQueryList | undefined;
  const onSystemChange = (): void => {
    if (preference === "system") {
      applyResolvedTheme(resolve("system"));
      emit();
    }
  };
  if (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function"
  ) {
    mql = window.matchMedia("(prefers-color-scheme: dark)");
    mql.addEventListener("change", onSystemChange);
  }

  return () => {
    listeners.delete(listener);
    mql?.removeEventListener("change", onSystemChange);
  };
}

function getPreferenceSnapshot(): ThemePreference {
  return preference;
}

function getServerSnapshot(): ThemePreference {
  return "system";
}

/**
 * Initialize the `data-theme` attribute as early as possible.
 * Called once from `main.tsx` before React mounts so there is no
 * light-mode flash before the first paint.
 */
export function initTheme(): void {
  preference = readStoredPreference();
  applyResolvedTheme(resolve(preference));
}

export interface UseThemeResult {
  /** The user's stored preference (may be `"system"`). */
  preference: ThemePreference;
  /** The concrete theme currently applied (`"light"` | `"dark"`). */
  resolvedTheme: ResolvedTheme;
  /** Set an explicit preference (persisted). */
  setPreference: (preference: ThemePreference) => void;
  /** Flip between light and dark based on what is currently shown. */
  toggleTheme: () => void;
}

export function useTheme(): UseThemeResult {
  const pref = useSyncExternalStore(
    subscribe,
    getPreferenceSnapshot,
    getServerSnapshot,
  );
  const resolvedTheme = resolve(pref);

  const toggleTheme = useCallback(() => {
    setPreference(resolve(getPreferenceSnapshot()) === "dark" ? "light" : "dark");
  }, []);

  return {
    preference: pref,
    resolvedTheme,
    setPreference,
    toggleTheme,
  };
}
