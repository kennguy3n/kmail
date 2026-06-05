/**
 * Tiny typed wrapper around `localStorage` for the client-side
 * persistence the WS2 feature set uses before a dedicated backend
 * lands (signatures, templates, label colours, delegation).
 *
 * Every value is namespaced under a single `kmail.` prefix so the
 * keys are easy to find/clear and never collide with other apps on
 * the same origin. Reads are defensive: a missing key returns the
 * caller's fallback, and a corrupt/non-JSON value is treated as
 * absent rather than throwing — losing one bad cache entry is
 * always preferable to wedging the UI.
 *
 * The functions degrade to no-ops / fallbacks when `localStorage`
 * is unavailable (SSR, privacy mode, quota errors) so callers never
 * have to guard their own try/catch.
 */

const PREFIX = "kmail.";

function namespaced(key: string): string {
  return `${PREFIX}${key}`;
}

function storage(): Storage | null {
  try {
    // Touch the global lazily — referencing it can throw in some
    // sandboxed iframes even before we call a method on it.
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

/** Read and JSON-parse a namespaced value, or return `fallback`. */
export function readJSON<T>(key: string, fallback: T): T {
  const store = storage();
  if (!store) return fallback;
  const raw = store.getItem(namespaced(key));
  if (raw === null) return fallback;
  try {
    return JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}

/** JSON-serialise and write a namespaced value. Returns success. */
export function writeJSON<T>(key: string, value: T): boolean {
  const store = storage();
  if (!store) return false;
  try {
    store.setItem(namespaced(key), JSON.stringify(value));
    return true;
  } catch {
    // Quota exceeded or serialisation cycle — surface as a failed
    // write rather than crashing the caller.
    return false;
  }
}

/** Remove a namespaced value. */
export function removeKey(key: string): void {
  const store = storage();
  if (!store) return;
  try {
    store.removeItem(namespaced(key));
  } catch {
    // ignore
  }
}

/**
 * Generate a stable-enough unique id for client-side records.
 * Prefers `crypto.randomUUID()` and falls back to a
 * timestamp+random string when the Web Crypto API is unavailable
 * (older jsdom, insecure contexts).
 */
export function newId(): string {
  try {
    if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
      return crypto.randomUUID();
    }
  } catch {
    // fall through
  }
  return `id-${Date.now().toString(36)}-${Math.random()
    .toString(36)
    .slice(2, 10)}`;
}
