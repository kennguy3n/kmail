/**
 * `useKeyboardShortcuts` — a small, dependency-free keyboard
 * shortcut engine.
 *
 * Supports both single keys (`"c"`, `"/"`, `"?"`) and Gmail-style
 * two-step sequences (`"g i"` = press `g` then `i`). Matching is
 * done on the produced character (`KeyboardEvent.key`), which keeps
 * symbol shortcuts like `"#"` and `"?"` correct across keyboard
 * layouts without hard-coding `shift+3` etc.
 *
 * Safety rails:
 *   - Events carrying Ctrl / Meta / Alt are ignored so we never
 *     hijack browser/OS chords (Ctrl+C, Cmd+L, …).
 *   - Shortcuts do not fire while focus is in a text input,
 *     textarea, select, or contenteditable element, unless the
 *     shortcut opts in with `allowInInput`.
 *
 * The hook is intentionally generic: the Layout registers global
 * shortcuts (compose, go-to-inbox, focus search, help), while each
 * page can register its own (reply, archive, …) without colliding.
 */
import { useEffect, useRef } from "react";

export interface KeyboardShortcut {
  /** Key or space-separated sequence, e.g. `"c"`, `"g i"`, `"?"`. */
  keys: string;
  /** Human-readable description shown in the help modal. */
  description: string;
  /** Optional grouping label for the help modal. */
  group?: string;
  /** Handler invoked when the shortcut matches. */
  handler: (event: KeyboardEvent) => void;
  /** Fire even while a text field is focused (default: false). */
  allowInInput?: boolean;
  /** Call `preventDefault()` on match (default: true). */
  preventDefault?: boolean;
}

interface UseKeyboardShortcutsOptions {
  /** Master switch — set false to disable (e.g. while a modal owns
   *  the keyboard). Defaults to true. */
  enabled?: boolean;
  /** Max gap between sequence steps before the buffer resets. */
  sequenceTimeoutMs?: number;
}

const DEFAULT_SEQUENCE_TIMEOUT_MS = 1200;

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  if (target.isContentEditable) return true;
  return false;
}

/** Split a `keys` spec into its lower-cased step sequence. */
function parseSteps(keys: string): string[] {
  return keys
    .trim()
    .split(/\s+/)
    .map((step) => step.toLowerCase());
}

/** Does `buffer` end with the exact `steps` sequence? */
function bufferEndsWith(buffer: string[], steps: string[]): boolean {
  if (steps.length > buffer.length) return false;
  const offset = buffer.length - steps.length;
  return steps.every((step, i) => buffer[offset + i] === step);
}

export function useKeyboardShortcuts(
  shortcuts: KeyboardShortcut[],
  options: UseKeyboardShortcutsOptions = {},
): void {
  const { enabled = true, sequenceTimeoutMs = DEFAULT_SEQUENCE_TIMEOUT_MS } =
    options;

  // Keep the latest shortcuts in a ref so the effect doesn't need to
  // re-bind the listener on every render (handlers are often inline).
  const shortcutsRef = useRef(shortcuts);
  shortcutsRef.current = shortcuts;

  useEffect(() => {
    if (!enabled) return;

    let buffer: string[] = [];
    let timer: ReturnType<typeof setTimeout> | undefined;

    const resetBuffer = (): void => {
      buffer = [];
    };

    const onKeyDown = (event: KeyboardEvent): void => {
      // Never intercept modifier chords — those belong to the
      // browser / OS.
      if (event.ctrlKey || event.metaKey || event.altKey) return;

      // Ignore bare modifier keypresses (Shift on its own, etc.).
      if (event.key.length !== 1 && event.key !== "Escape") {
        // Allow only single-character keys through the matcher;
        // multi-char keys ("Shift", "ArrowDown", …) are not part of
        // our shortcut vocabulary and would pollute the sequence
        // buffer. We do let them reset nothing (no-op).
        return;
      }

      const key = event.key.toLowerCase();
      buffer.push(key);
      if (buffer.length > 8) buffer = buffer.slice(-8);

      const editable = isEditableTarget(event.target);

      for (const shortcut of shortcutsRef.current) {
        if (editable && !shortcut.allowInInput) continue;
        const steps = parseSteps(shortcut.keys);
        if (steps.length === 0) continue;
        if (bufferEndsWith(buffer, steps)) {
          if (shortcut.preventDefault !== false) event.preventDefault();
          shortcut.handler(event);
          resetBuffer();
          break;
        }
      }

      // Restart the inactivity timer that clears partial sequences.
      if (timer) clearTimeout(timer);
      timer = setTimeout(resetBuffer, sequenceTimeoutMs);
    };

    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      if (timer) clearTimeout(timer);
    };
  }, [enabled, sequenceTimeoutMs]);
}
