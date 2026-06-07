import { useMemo, useState } from "react";

import { defaultSnoozePresets } from "../../api/snooze";

/**
 * SnoozePicker is the popover the user sees after clicking the
 * "Snooze" action on an email row (or the toolbar in
 * MessageView). It offers four quick presets — "Later today",
 * "Tomorrow morning", "This weekend", "Next week" — and an
 * arbitrary date/time input as the escape hatch.
 *
 * The component is presentational: it surfaces the chosen
 * absolute timestamp through `onPick`. The caller is responsible
 * for the JMAP move + persistence (Inbox.tsx / MessageView.tsx
 * both route through `snoozeEmail`).
 */
export interface SnoozePickerProps {
  onPick: (until: Date) => void;
  onCancel: () => void;
  /**
   * Optional clock injection for tests. Production code omits
   * this so the live wall clock is used.
   */
  now?: () => Date;
}

export default function SnoozePicker({
  onPick,
  onCancel,
  now = () => new Date(),
}: SnoozePickerProps) {
  const presets = useMemo(() => defaultSnoozePresets(), []);
  // `customValue` holds the user's free-form picker input. Like
  // Compose.tsx we use `datetime-local` which gives wall-clock
  // input matching the user's tz — we convert to a Date at
  // submit time to anchor the absolute instant.
  const [customValue, setCustomValue] = useState("");
  const [error, setError] = useState<string | null>(null);

  function handlePreset(label: string, resolve: (n: Date) => Date) {
    const target = resolve(now());
    if (Number.isNaN(target.getTime())) {
      setError(`Could not resolve "${label}" to a timestamp`);
      return;
    }
    if (target.getTime() <= now().getTime() + 30_000) {
      setError(`"${label}" is in the past — pick another time`);
      return;
    }
    onPick(target);
  }

  function handleCustom() {
    if (!customValue) {
      setError("Pick a date and time first");
      return;
    }
    const target = new Date(customValue);
    if (Number.isNaN(target.getTime())) {
      setError("Invalid date/time");
      return;
    }
    // Server requires >= 1 minute horizon (MinSnoozeHorizon).
    // Reject early so the user sees a focused message instead of
    // a 400 from the REST call.
    if (target.getTime() < now().getTime() + 60_000) {
      setError("Snooze must be at least one minute away");
      return;
    }
    onPick(target);
  }

  return (
    <div role="dialog" aria-label="Snooze" className={styles.root}>
      <p className={styles.title}>Snooze until…</p>
      <ul className={styles.list}>
        {presets.map((preset) => (
          <li key={preset.label}>
            <button
              type="button"
              className={styles.preset}
              onClick={() => handlePreset(preset.label, preset.resolve)}
            >
              {preset.label}
            </button>
          </li>
        ))}
      </ul>
      <div className={styles.customRow}>
        <input
          type="datetime-local"
          value={customValue}
          onChange={(e) => setCustomValue(e.target.value)}
          aria-label="Custom snooze date and time"
          className={styles.customInput}
        />
        <button type="button" onClick={handleCustom} className={styles.customGo}>
          Pick
        </button>
      </div>
      {error && <p className={styles.error}>{error}</p>}
      <div className={styles.footer}>
        <button type="button" onClick={onCancel} className={styles.cancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}

/** Theme-aware Tailwind class recipes for the SnoozePicker popover. */
const styles: Record<string, string> = {
  root: "absolute right-0 top-full z-dropdown min-w-[240px] rounded-lg border border-border bg-elevated p-3 shadow-lg",
  title: "mb-2 mt-0 text-sm font-semibold",
  list: "m-0 flex list-none flex-col gap-0.5 p-0",
  preset:
    "w-full cursor-pointer rounded-md border-0 bg-transparent px-2 py-1.5 text-left text-sm transition-colors hover:bg-surface-hover",
  customRow: "mt-2 flex gap-1",
  customInput:
    "flex-1 rounded-md border border-border bg-surface px-1.5 py-1 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  customGo:
    "cursor-pointer rounded-md border-0 bg-primary px-2.5 py-1 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  footer: "mt-2 text-right",
  cancel:
    "cursor-pointer rounded-md border border-border bg-transparent px-2 py-1 text-sm text-fg transition-colors hover:bg-surface-hover",
  error: "m-0 mt-2 text-xs text-danger-fg",
};
