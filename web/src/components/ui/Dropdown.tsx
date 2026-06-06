import {
  cloneElement,
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
import type { KeyboardEvent, ReactElement, ReactNode } from "react";

import { cn } from "../../lib/cn";

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
    <div ref={containerRef} className={cn("relative inline-block", className)}>
      {triggerNode}
      {open && (
        <div
          role="menu"
          id={menuId}
          aria-label={ariaLabel}
          className={cn(
            "absolute top-[calc(100%+0.375rem)] z-dropdown min-w-44 origin-top animate-scale-in rounded-lg border border-border bg-elevated p-1 shadow-lg",
            align === "end" ? "right-0" : "left-0",
          )}
          onKeyDown={onMenuKeyDown}
        >
          {items.map((item, i) => (
            <div key={item.id}>
              {item.separatorBefore && (
                <div className="my-1 h-px bg-border" role="separator" />
              )}
              <button
                ref={(el) => {
                  itemRefs.current[i] = el;
                }}
                type="button"
                role="menuitem"
                tabIndex={-1}
                disabled={item.disabled}
                className={cn(
                  "flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-left text-sm text-fg outline-none transition-colors hover:bg-surface-hover focus-visible:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-50 [&>span>svg]:size-4",
                  item.danger &&
                    "text-danger-fg hover:bg-danger-bg focus-visible:bg-danger-bg",
                )}
                onClick={() => {
                  item.onSelect?.();
                  close(true);
                }}
              >
                {item.icon && (
                  <span
                    className={cn(
                      "inline-flex shrink-0",
                      item.danger ? "text-danger-fg" : "text-fg-muted",
                    )}
                    aria-hidden="true"
                  >
                    {item.icon}
                  </span>
                )}
                <span className="flex-1 truncate">{item.label}</span>
              </button>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
