import { Modal } from "./ui/Modal";
import type { KeyboardShortcut } from "../hooks/useKeyboardShortcuts";

export interface ShortcutHelpModalProps {
  open: boolean;
  onClose: () => void;
  shortcuts: KeyboardShortcut[];
}

/** Render a key spec ("g i", "?") as individual <kbd> chips. */
function renderKeys(keys: string): JSX.Element {
  const steps = keys.trim().split(/\s+/);
  return (
    <span className="inline-flex items-center gap-1">
      {steps.map((step, i) => (
        <kbd
          key={i}
          className="inline-flex min-w-6 items-center justify-center rounded-md border border-border bg-surface-muted px-1.5 py-0.5 font-mono text-xs font-medium text-fg shadow-sm"
        >
          {step}
        </kbd>
      ))}
    </span>
  );
}

/**
 * ShortcutHelpModal — the `?`-triggered cheat sheet. Groups the
 * registered shortcuts by their `group` label so the list stays
 * legible as more pages register their own bindings.
 */
export function ShortcutHelpModal({
  open,
  onClose,
  shortcuts,
}: ShortcutHelpModalProps): JSX.Element | null {
  const groups = new Map<string, KeyboardShortcut[]>();
  for (const shortcut of shortcuts) {
    const group = shortcut.group ?? "General";
    const list = groups.get(group) ?? [];
    list.push(shortcut);
    groups.set(group, list);
  }

  return (
    <Modal open={open} onClose={onClose} title="Keyboard shortcuts" size="md">
      <div className="flex flex-col gap-5">
        {Array.from(groups.entries()).map(([group, list]) => (
          <section key={group}>
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-fg-muted">
              {group}
            </h3>
            <dl className="flex flex-col">
              {list.map((shortcut) => (
                <div
                  key={shortcut.keys}
                  className="flex items-center justify-between gap-4 border-b border-border/70 py-2 last:border-0"
                >
                  <dt className="text-sm text-fg">{shortcut.description}</dt>
                  <dd className="flex-none">{renderKeys(shortcut.keys)}</dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
    </Modal>
  );
}
