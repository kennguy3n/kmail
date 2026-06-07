import { useId, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";

import { cn } from "../../lib/cn";

export interface TabItem {
  id: string;
  label: ReactNode;
  content: ReactNode;
  disabled?: boolean;
}

export interface TabsProps {
  items: TabItem[];
  /** Controlled active tab id. */
  value?: string;
  /** Initial active tab id (uncontrolled). Defaults to first enabled. */
  defaultValue?: string;
  onChange?: (id: string) => void;
  /** Accessible label for the tablist. */
  ariaLabel?: string;
  className?: string;
}

/**
 * Tabs — an accessible tab set following the WAI-ARIA Tabs pattern:
 * `role="tablist"`/`tab`/`tabpanel` wiring, roving tabindex, and
 * Left/Right/Home/End keyboard navigation.
 */
export function Tabs({
  items,
  value,
  defaultValue,
  onChange,
  ariaLabel,
  className,
}: TabsProps): JSX.Element {
  const baseId = useId();
  const firstEnabled = items.find((t) => !t.disabled)?.id ?? items[0]?.id;
  const [internal, setInternal] = useState(defaultValue ?? firstEnabled);
  const active = value ?? internal;
  const tabRefs = useRef<Record<string, HTMLButtonElement | null>>({});

  const select = (id: string): void => {
    if (value === undefined) setInternal(id);
    onChange?.(id);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLButtonElement>): void => {
    const enabled = items.filter((t) => !t.disabled);
    const currentIndex = enabled.findIndex((t) => t.id === active);
    if (currentIndex === -1) return;

    let nextIndex = currentIndex;
    switch (event.key) {
      case "ArrowRight":
      case "ArrowDown":
        nextIndex = (currentIndex + 1) % enabled.length;
        break;
      case "ArrowLeft":
      case "ArrowUp":
        nextIndex = (currentIndex - 1 + enabled.length) % enabled.length;
        break;
      case "Home":
        nextIndex = 0;
        break;
      case "End":
        nextIndex = enabled.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    const nextId = enabled[nextIndex].id;
    select(nextId);
    tabRefs.current[nextId]?.focus();
  };

  const activeItem = items.find((t) => t.id === active);

  return (
    <div className={cn("flex flex-col", className)}>
      <div
        role="tablist"
        aria-label={ariaLabel}
        className="flex items-center gap-1 border-b border-border"
      >
        {items.map((tab) => {
          const selected = tab.id === active;
          return (
            <button
              key={tab.id}
              ref={(el) => {
                tabRefs.current[tab.id] = el;
              }}
              type="button"
              role="tab"
              id={`${baseId}-tab-${tab.id}`}
              aria-selected={selected}
              aria-controls={`${baseId}-panel-${tab.id}`}
              tabIndex={selected ? 0 : -1}
              disabled={tab.disabled}
              className={cn(
                "-mb-px border-b-2 px-3.5 py-2.5 text-sm font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset disabled:cursor-not-allowed disabled:opacity-50",
                selected
                  ? "border-primary text-primary"
                  : "border-transparent text-fg-muted hover:border-border-strong hover:text-fg",
              )}
              onClick={() => select(tab.id)}
              onKeyDown={onKeyDown}
            >
              {tab.label}
            </button>
          );
        })}
      </div>
      {activeItem && (
        <div
          role="tabpanel"
          id={`${baseId}-panel-${activeItem.id}`}
          aria-labelledby={`${baseId}-tab-${activeItem.id}`}
          tabIndex={0}
          className="pt-4 outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {activeItem.content}
        </div>
      )}
    </div>
  );
}
