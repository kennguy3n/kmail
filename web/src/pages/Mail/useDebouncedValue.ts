import { useEffect, useState } from "react";

/**
 * Return a debounced copy of `value` that only updates after
 * `delayMs` of quiet. Used to throttle the GAL typeahead search so
 * keystrokes don't fire a request each. The timer is cleared on
 * every change and on unmount, so a fast typist never leaves a
 * stale request scheduled.
 */
export function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState<T>(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(t);
  }, [value, delayMs]);
  return debounced;
}
