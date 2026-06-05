import { useEffect, useMemo, useRef, useState } from "react";

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
  inputStyle?: React.CSSProperties;
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
  inputStyle,
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
    <div ref={containerRef} style={styles.wrap}>
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
        style={{ ...styles.input, ...inputStyle }}
      />
      {open && suggestions.length > 0 && (
        <ul style={styles.dropdown} role="listbox">
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
                style={{
                  ...styles.option,
                  ...(i === activeIndex ? styles.optionActive : {}),
                }}
              >
                <span style={styles.avatar} aria-hidden="true">
                  {initials(entry)}
                </span>
                <span style={styles.optionText}>
                  <span style={styles.optionName}>
                    {entry.display_name || entry.email}
                  </span>
                  {entry.display_name && (
                    <span style={styles.optionEmail}>{entry.email}</span>
                  )}
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {loading && <span style={styles.loading}>…</span>}
    </div>
  );
}

const styles: Record<string, React.CSSProperties> = {
  wrap: { position: "relative", flex: 1 },
  input: {
    width: "100%",
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    boxSizing: "border-box",
  },
  dropdown: {
    position: "absolute",
    zIndex: 20,
    top: "100%",
    left: 0,
    right: 0,
    margin: "0.15rem 0 0",
    padding: "0.25rem",
    listStyle: "none",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.375rem",
    boxShadow: "0 6px 18px rgba(0,0,0,0.12)",
    maxHeight: "260px",
    overflowY: "auto",
  },
  option: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    width: "100%",
    padding: "0.35rem 0.5rem",
    background: "transparent",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
    textAlign: "left",
  },
  optionActive: { background: "#eff6ff" },
  avatar: {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: "1.75rem",
    height: "1.75rem",
    borderRadius: "999px",
    background: "#2563eb",
    color: "#fff",
    fontSize: "0.7rem",
    fontWeight: 700,
    flexShrink: 0,
  },
  optionText: { display: "flex", flexDirection: "column", overflow: "hidden" },
  optionName: {
    fontSize: "0.85rem",
    color: "#111827",
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
  },
  optionEmail: {
    fontSize: "0.75rem",
    color: "#6b7280",
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
  },
  loading: {
    position: "absolute",
    right: "0.5rem",
    top: "0.4rem",
    color: "#9ca3af",
    fontSize: "0.9rem",
  },
};
