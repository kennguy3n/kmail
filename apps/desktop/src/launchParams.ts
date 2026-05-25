// Snapshot of the launch-time `window.location.hash`, captured at
// module-evaluation time *before* `HashRouter` mounts.
//
// **Why this exists.** The desktop renderer reads the SDK
// session parameters (bff URL, bearer token, database path,
// account id) from the URL hash that KChat puts on the launch
// URL:
//
//   kmail.html#bff=https://...&token=...&db=...&acct=...
//
// But `<HashRouter>` (used because production loads from
// `file://`) claims ownership of `window.location.hash` for
// routing and normalises the hash to start with `#/`. If
// `App.tsx` read `window.location.hash` after the router
// mounted it would either see no routes matching (when the
// hash doesn't start with `/`) or see a leading `/` glued to
// the first param key (e.g. `URLSearchParams('/bff=...')` parses
// the first key as `/bff`, not `bff`). Both break the
// production launch.
//
// **The fix.** Snapshot the hash exactly once, at module load
// time, before React even renders. The snapshot lives in this
// module's top-level scope so it survives every subsequent
// HashRouter mutation. `App.tsx` reads the snapshot instead of
// re-reading `window.location.hash`.
//
// **Production / dev fallback.** When no hash params are
// present (e.g. dev `npm run dev` against a stub KChat),
// `App.tsx` falls back to the `VITE_KMAIL_*` env vars Vite
// injects at build time.

function snapshot(): URLSearchParams {
  if (typeof window === 'undefined') return new URLSearchParams();
  // Strip the leading `#` only. Don't strip any leading `/` —
  // if HashRouter had already run and prepended `/` we'd want to
  // surface the bug rather than silently mangle the launch URL.
  // This function executes before HashRouter mounts, so the raw
  // value should be free of routing prefixes by construction.
  return new URLSearchParams(window.location.hash.replace(/^#/, ''));
}

export const launchHashParams: URLSearchParams = snapshot();

/**
 * Test-only escape hatch: re-evaluate the hash snapshot. Production
 * code never calls this — the snapshot is meant to be a one-shot
 * read locked in at module load.
 */
export function __reseedLaunchHashForTests(raw: string): void {
  // Replace the prototype-level entries on `launchHashParams` so
  // existing imports continue to see the new values. We mutate
  // in place because tests sometimes import `launchHashParams`
  // before they get a chance to seed `window.location.hash`.
  for (const key of Array.from(launchHashParams.keys())) {
    launchHashParams.delete(key);
  }
  const reseeded = new URLSearchParams(raw.replace(/^#/, ''));
  for (const [k, v] of reseeded.entries()) {
    launchHashParams.append(k, v);
  }
}
