import { useState } from "react";
import { Link } from "react-router-dom";

import { cn } from "../../lib/cn";

import {
  createLabel,
  deleteLabel,
  LABEL_COLORS,
  listLabels,
  updateLabel,
} from "../../api/labels";
import type { Label } from "../../types";

/**
 * Label management page.
 *
 * CRUD over the {@link listLabels} registry. Each label maps to a
 * stable JMAP keyword (generated once at creation); the Inbox
 * applies/removes labels by toggling that keyword on the email.
 * Renaming or recolouring a label here never changes its keyword,
 * so already-labelled emails keep their label.
 */
export default function Labels() {
  const [labels, setLabels] = useState<Label[]>(() => listLabels());
  const [name, setName] = useState("");
  const [color, setColor] = useState<string>(LABEL_COLORS[5]);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editColor, setEditColor] = useState<string>(LABEL_COLORS[5]);

  const refresh = () => setLabels(listLabels());

  const onCreate = () => {
    if (name.trim() === "") return;
    createLabel({ name, color });
    setName("");
    setColor(LABEL_COLORS[5]);
    refresh();
  };

  const startEdit = (label: Label) => {
    setEditingId(label.id);
    setEditName(label.name);
    setEditColor(label.color);
  };

  const onSaveEdit = () => {
    if (!editingId) return;
    updateLabel(editingId, { name: editName, color: editColor });
    setEditingId(null);
    refresh();
  };

  const onDelete = (id: string) => {
    deleteLabel(id);
    if (editingId === id) setEditingId(null);
    refresh();
  };

  return (
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Labels</h2>
        <Link to="/mail" className={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div className={styles.createRow}>
        <input
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="New label name"
          aria-label="New label name"
          className={styles.input}
          onKeyDown={(e) => {
            if (e.key === "Enter") onCreate();
          }}
        />
        <ColorPicker value={color} onChange={setColor} ariaPrefix="New label" />
        <button type="button" onClick={onCreate} className={styles.primaryButton}>
          Add label
        </button>
      </div>

      {labels.length === 0 ? (
        <p className={styles.muted}>No labels yet.</p>
      ) : (
        <ul className={styles.list}>
          {labels.map((label) => (
            <li key={label.id} className={styles.listItem}>
              {editingId === label.id ? (
                <>
                  <input
                    type="text"
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                    aria-label={`Rename ${label.name}`}
                    className={styles.input}
                  />
                  <ColorPicker
                    value={editColor}
                    onChange={setEditColor}
                    ariaPrefix={`Edit ${label.name}`}
                  />
                  <button
                    type="button"
                    onClick={onSaveEdit}
                    className={styles.primaryButton}
                  >
                    Save
                  </button>
                  <button
                    type="button"
                    onClick={() => setEditingId(null)}
                    className={styles.secondaryButton}
                  >
                    Cancel
                  </button>
                </>
              ) : (
                <>
                  <span className={styles.chip}>
                    <span
                      className={styles.dot}
                      style={{ background: label.color }}
                      aria-hidden="true"
                    />
                    {label.name}
                  </span>
                  <span className={styles.spacer} />
                  <button
                    type="button"
                    onClick={() => startEdit(label)}
                    className={styles.secondaryButton}
                  >
                    Edit
                  </button>
                  <button
                    type="button"
                    onClick={() => onDelete(label.id)}
                    className={styles.dangerButton}
                  >
                    Delete
                  </button>
                </>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function ColorPicker({
  value,
  onChange,
  ariaPrefix,
}: {
  value: string;
  onChange: (color: string) => void;
  ariaPrefix: string;
}) {
  return (
    <span className={styles.colorRow}>
      {LABEL_COLORS.map((c) => (
        <button
          key={c}
          type="button"
          onClick={() => onChange(c)}
          aria-label={`${ariaPrefix} colour ${c}`}
          aria-pressed={value === c}
          title={c}
          className={cn(
            styles.swatch,
            value === c && "outline outline-2 outline-fg",
          )}
          style={{ background: c }}
        />
      ))}
    </span>
  );
}

/** Theme-aware Tailwind class recipes for the Labels manager. */
const styles: Record<string, string> = {
  root: "max-w-[720px] p-4",
  header: "mb-4 flex items-baseline justify-between",
  title: "m-0 text-xl font-semibold",
  backLink: "text-sm text-primary no-underline hover:underline",
  createRow: "mb-4 flex flex-wrap items-center gap-2",
  input:
    "rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  colorRow: "inline-flex flex-wrap gap-1",
  swatch:
    "size-5 cursor-pointer rounded-sm border border-black/10 p-0",
  list: "m-0 grid list-none gap-1.5 p-0",
  listItem:
    "flex items-center gap-2 rounded-md border border-border bg-surface px-2 py-1.5",
  chip: "inline-flex items-center gap-1.5 text-sm font-semibold text-fg",
  dot: "inline-block size-3.5 rounded-pill",
  spacer: "flex-1",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3 py-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  secondaryButton:
    "cursor-pointer rounded-md border border-border bg-surface px-3 py-1.5 text-sm text-fg transition-colors hover:bg-surface-hover",
  dangerButton:
    "cursor-pointer rounded-md border border-danger/40 bg-surface px-3 py-1.5 text-sm text-danger-fg transition-colors hover:bg-danger-bg",
  muted: "text-sm italic text-fg-muted",
};
