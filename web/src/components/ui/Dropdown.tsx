import {
  cloneElement,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import type { KeyboardEvent, ReactElement, ReactNode } from "react";

import styles from "./Dropdown.module.css";

export interface DropdownItem {
  id: string;
  label: ReactNode;
  icon?: ReactNode;
  onSelect?: () => void;
  disabled?: boolean;
  danger?: boolean;
  /** Render a separator before this item. */
  separatorBefore?: boolean;
}

export interface DropdownProps {
  /** Trigger element; cloned to receive aria-* + onClick wiring. */
  trigger: ReactElement;
  items: DropdownItem[];
  /** Menu alignment relative to the trigger. */
  align?: "start" | "end";
  /** Accessible label for the menu. */
  ariaLabel?: string;
  className?: string;
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Dropdown — an accessible menu button (WAI-ARIA menu pattern).
 * Closes on outside click and Escape, supports Up/Down/Home/End
 * roving focus, and restores focus to the trigger on close.
 */
export function Dropdown({
  trigger,
  items,
  align = "start",
  ariaLabel,
  className,
}: DropdownProps): JSX.Element {
  const [open, setOpen] = useState(false);
  const menuId = useId();
  const containerRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLElement>(null);
  const itemRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const wasOpen = useRef(false);

  const enabledIndexes = items
    .map((item, i) => (item.disabled ? -1 : i))
    .filter((i) => i >= 0);

  const close = useCallback((restoreFocus: boolean) => {
    setOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) return;
    const onDocClick = (e: MouseEvent): void => {
      if (!containerRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, [open]);

  useEffect(() => {
    // Focus the first enabled item only on the closed→open transition.
    // Guarding on the previous open state keeps `items` in the
    // dependency list (so the lookup stays honest) without stealing
    // focus back whenever the parent re-renders with a fresh `items`
    // array while the menu is already open.
    if (open && !wasOpen.current) {
      const firstEnabled = items.findIndex((item) => !item.disabled);
      if (firstEnabled >= 0) itemRefs.current[firstEnabled]?.focus();
    }
    wasOpen.current = open;
  }, [open, items]);

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    // While the menu is open it owns the keyboard: stop keydowns from
    // bubbling to window-level listeners (e.g. the global keyboard
    // shortcut engine) so pressing "c" or "/" inside the menu can't
    // navigate away while the menu stays visually open.
    event.stopPropagation();
    if (event.key === "Escape") {
      event.preventDefault();
      close(true);
      return;
    }
    // Nothing focusable: don't swallow the key (avoids NaN index math
    // and lets the keypress behave normally for an all-disabled menu).
    if (enabledIndexes.length === 0) return;
    const focusedIndex = itemRefs.current.findIndex(
      (el) => el === document.activeElement,
    );
    const posInEnabled = enabledIndexes.indexOf(focusedIndex);
    let nextPos = posInEnabled;
    switch (event.key) {
      case "ArrowDown":
        nextPos = (posInEnabled + 1) % enabledIndexes.length;
        break;
      case "ArrowUp":
        nextPos =
          (posInEnabled - 1 + enabledIndexes.length) % enabledIndexes.length;
        break;
      case "Home":
        nextPos = 0;
        break;
      case "End":
        nextPos = enabledIndexes.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    itemRefs.current[enabledIndexes[nextPos]]?.focus();
  };

  const triggerNode = cloneElement(trigger, {
    ref: triggerRef,
    "aria-haspopup": "menu",
    "aria-expanded": open,
    onClick: (e: React.MouseEvent) => {
      trigger.props.onClick?.(e);
      setOpen((v) => !v);
    },
  });

  return (
    <div ref={containerRef} className={cx(styles.dropdown, className)}>
      {triggerNode}
      {open && (
        <div
          role="menu"
          id={menuId}
          aria-label={ariaLabel}
          className={cx(styles.menu, align === "end" && styles.alignEnd)}
          onKeyDown={onMenuKeyDown}
        >
          {items.map((item, i) => (
            <div key={item.id} className={styles.itemWrap}>
              {item.separatorBefore && (
                <div className={styles.separator} role="separator" />
              )}
              <button
                ref={(el) => {
                  itemRefs.current[i] = el;
                }}
                type="button"
                role="menuitem"
                tabIndex={-1}
                disabled={item.disabled}
                className={cx(styles.item, item.danger && styles.danger)}
                onClick={() => {
                  item.onSelect?.();
                  close(true);
                }}
              >
                {item.icon && (
                  <span className={styles.icon} aria-hidden="true">
                    {item.icon}
                  </span>
                )}
                <span className={styles.itemLabel}>{item.label}</span>
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
