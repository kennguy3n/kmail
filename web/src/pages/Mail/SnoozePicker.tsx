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
    <div role="dialog" aria-label="Snooze" style={styles.root}>
      <p style={styles.title}>Snooze until…</p>
      <ul style={styles.list}>
        {presets.map((preset) => (
          <li key={preset.label}>
            <button
              type="button"
              style={styles.preset}
              onClick={() => handlePreset(preset.label, preset.resolve)}
            >
              {preset.label}
            </button>
          </li>
        ))}
      </ul>
      <div style={styles.customRow}>
        <input
          type="datetime-local"
          value={customValue}
          onChange={(e) => setCustomValue(e.target.value)}
          aria-label="Custom snooze date and time"
          style={styles.customInput}
        />
        <button type="button" onClick={handleCustom} style={styles.customGo}>
          Pick
        </button>
      </div>
      {error && <p style={styles.error}>{error}</p>}
      <div style={styles.footer}>
        <button type="button" onClick={onCancel} style={styles.cancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}

const styles: Record<string, React.CSSProperties> = {
  root: {
    position: "absolute",
    right: 0,
    top: "100%",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.5rem",
    boxShadow: "0 8px 24px rgba(0,0,0,0.12)",
    padding: "0.75rem",
    minWidth: "240px",
    zIndex: 10,
  },
  title: {
    margin: 0,
    fontWeight: 600,
    fontSize: "0.85rem",
    marginBottom: "0.5rem",
  },
  list: {
    listStyle: "none",
    margin: 0,
    padding: 0,
    display: "flex",
    flexDirection: "column",
    gap: "0.125rem",
  },
  preset: {
    width: "100%",
    textAlign: "left",
    padding: "0.4rem 0.5rem",
    background: "transparent",
    border: "none",
    cursor: "pointer",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
  },
  customRow: {
    display: "flex",
    gap: "0.25rem",
    marginTop: "0.5rem",
  },
  customInput: {
    flex: 1,
    padding: "0.25rem 0.4rem",
    fontSize: "0.85rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  customGo: {
    padding: "0.25rem 0.6rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
    cursor: "pointer",
  },
  footer: {
    marginTop: "0.5rem",
    textAlign: "right",
  },
  cancel: {
    padding: "0.2rem 0.5rem",
    background: "transparent",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  error: {
    color: "#b91c1c",
    fontSize: "0.8rem",
    margin: "0.5rem 0 0 0",
  },
};
