import { useEffect, useMemo, useRef, useState } from "react";

import { cn } from "../../lib/cn";
import { searchGlobalAddressList } from "../../api/contacts";
import type { GalEntry } from "../../types";
import { useDebouncedValue } from "./useDebouncedValue";

/**
 * Recipient input with Global Address List (GAL) autocomplete.
 *
 * Wraps a plain comma-separated recipient field (the shape Compose
 * already parses) and adds a debounced typeahead against
 * `GET /api/v1/contacts/gal/search`. Only the token after the last
 * comma is treated as the active query, so multi-recipient entry
 * keeps working — picking a suggestion replaces just that token and
 * appends `", "` ready for the next address.
 *
 * The component is deliberately storage- and JMAP-agnostic: it
 * takes the resolved `tenantId` (the GAL is a tenant-scoped admin
 * resource) and emits string changes, leaving address parsing to
 * the caller. When no tenant is selected it degrades to a plain
 * input with no suggestions rather than erroring.
 */
export interface ContactPickerProps {
  id?: string;
  value: string;
  onChange: (value: string) => void;
  tenantId: string | null;
  placeholder?: string;
  required?: boolean;
  ariaLabel?: string;
  /** Extra Tailwind classes merged onto the text input. */
  inputClassName?: string;
}

/** Split the field into the committed prefix and the active token. */
function splitActiveToken(value: string): { prefix: string; token: string } {
  const idx = value.lastIndexOf(",");
  if (idx === -1) return { prefix: "", token: value.trimStart() };
  return {
    prefix: value.slice(0, idx + 1),
    token: value.slice(idx + 1).trimStart(),
  };
}

function formatEntry(entry: GalEntry): string {
  return entry.display_name
    ? `${entry.display_name} <${entry.email}>`
    : entry.email;
}

function initials(entry: GalEntry): string {
  const source = entry.display_name || entry.email;
  const parts = source.trim().split(/\s+/).slice(0, 2);
  const letters = parts.map((p) => p[0]).join("");
  return (letters || source[0] || "?").toUpperCase();
}

export default function ContactPicker({
  id,
  value,
  onChange,
  tenantId,
  placeholder,
  required,
  ariaLabel,
  inputClassName,
}: ContactPickerProps) {
  const [open, setOpen] = useState(false);
  const [suggestions, setSuggestions] = useState<GalEntry[]>([]);
  const [activeIndex, setActiveIndex] = useState(0);
  const [loading, setLoading] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  const { token } = useMemo(() => splitActiveToken(value), [value]);
  const debouncedToken = useDebouncedValue(token, 200);

  // Fetch suggestions for the active token. Skips queries shorter
  // than 2 chars (too noisy) and when no tenant is available.
  useEffect(() => {
    const q = debouncedToken.trim();
    if (!tenantId || q.length < 2) {
      setSuggestions([]);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    searchGlobalAddressList(tenantId, q)
      .then((results) => {
        if (cancelled) return;
        setSuggestions(results.slice(0, 8));
        setActiveIndex(0);
        setOpen(results.length > 0);
      })
      .catch(() => {
        if (!cancelled) setSuggestions([]);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [debouncedToken, tenantId]);

  // Close the dropdown on outside click.
  useEffect(() => {
    if (!open) return;
    const onDocClick = (e: MouseEvent) => {
      if (
        containerRef.current &&
        !containerRef.current.contains(e.target as Node)
      ) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, [open]);

  const choose = (entry: GalEntry) => {
    const { prefix } = splitActiveToken(value);
    const sep = prefix && !prefix.endsWith(" ") ? " " : "";
    onChange(`${prefix}${sep}${formatEntry(entry)}, `);
    setOpen(false);
    setSuggestions([]);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open || suggestions.length === 0) return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActiveIndex((i) => (i + 1) % suggestions.length);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActiveIndex((i) => (i - 1 + suggestions.length) % suggestions.length);
    } else if (e.key === "Enter") {
      // Only intercept Enter when a suggestion is highlighted so the
      // form's normal submit still works when the dropdown is shut.
      e.preventDefault();
      choose(suggestions[activeIndex]);
    } else if (e.key === "Escape") {
      setOpen(false);
    }
  };

  return (
    <div ref={containerRef} className="relative flex-1">
      <input
        id={id}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={onKeyDown}
        onFocus={() => suggestions.length > 0 && setOpen(true)}
        placeholder={placeholder}
        required={required}
        aria-label={ariaLabel}
        autoComplete="off"
        role="combobox"
        aria-expanded={open}
        aria-autocomplete="list"
        className={cn(
          "box-border w-full rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
          inputClassName,
        )}
      />
      {open && suggestions.length > 0 && (
        <ul
          className="absolute inset-x-0 top-full z-dropdown m-0 mt-0.5 max-h-[260px] list-none overflow-y-auto rounded-lg border border-border bg-elevated p-1 shadow-lg"
          role="listbox"
        >
          {suggestions.map((entry, i) => (
            <li key={entry.email} role="option" aria-selected={i === activeIndex}>
              <button
                type="button"
                // Use mousedown so the click registers before the
                // input's blur closes the dropdown.
                onMouseDown={(e) => {
                  e.preventDefault();
                  choose(entry);
                }}
                className={cn(
                  "flex w-full cursor-pointer items-center gap-2 rounded-md border-0 bg-transparent px-2 py-1.5 text-left transition-colors hover:bg-surface-hover",
                  i === activeIndex && "bg-surface-hover",
                )}
              >
                <span
                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-pill bg-primary text-[0.7rem] font-bold text-primary-fg"
                  aria-hidden="true"
                >
                  {initials(entry)}
                </span>
                <span className="flex flex-col overflow-hidden">
                  <span className="truncate text-sm text-fg">
                    {entry.display_name || entry.email}
                  </span>
                  {entry.display_name && (
                    <span className="truncate text-xs text-fg-muted">
                      {entry.email}
                    </span>
                  )}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {loading && (
        <span className="absolute right-2 top-1.5 text-sm text-fg-subtle">…</span>
      )}
    </div>
  );
}

