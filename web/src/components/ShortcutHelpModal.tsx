import { Modal } from "./ui/Modal";
import type { KeyboardShortcut } from "../hooks/useKeyboardShortcuts";
import styles from "./ShortcutHelpModal.module.css";

export interface ShortcutHelpModalProps {
  open: boolean;
  onClose: () => void;
  shortcuts: KeyboardShortcut[];
}

/** Render a key spec ("g i", "?") as individual <kbd> chips. */
function renderKeys(keys: string): JSX.Element {
  const steps = keys.trim().split(/\s+/);
  return (
    <span className={styles.keys}>
      {steps.map((step, i) => (
        <kbd key={i} className={styles.kbd}>
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
      <div className={styles.groups}>
        {Array.from(groups.entries()).map(([group, list]) => (
          <section key={group} className={styles.group}>
            <h3 className={styles.groupTitle}>{group}</h3>
            <dl className={styles.list}>
              {list.map((shortcut) => (
                <div key={shortcut.keys} className={styles.row}>
                  <dt className={styles.description}>
                    {shortcut.description}
                  </dt>
                  <dd className={styles.keyCell}>
                    {renderKeys(shortcut.keys)}
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
    </Modal>
  );
}
