import { useState } from "react";
import { Link } from "react-router-dom";

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
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Labels</h2>
        <Link to="/mail" style={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div style={styles.createRow}>
        <input
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="New label name"
          aria-label="New label name"
          style={styles.input}
          onKeyDown={(e) => {
            if (e.key === "Enter") onCreate();
          }}
        />
        <ColorPicker value={color} onChange={setColor} ariaPrefix="New label" />
        <button type="button" onClick={onCreate} style={styles.primaryButton}>
          Add label
        </button>
      </div>

      {labels.length === 0 ? (
        <p style={styles.muted}>No labels yet.</p>
      ) : (
        <ul style={styles.list}>
          {labels.map((label) => (
            <li key={label.id} style={styles.listItem}>
              {editingId === label.id ? (
                <>
                  <input
                    type="text"
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                    aria-label={`Rename ${label.name}`}
                    style={styles.input}
                  />
                  <ColorPicker
                    value={editColor}
                    onChange={setEditColor}
                    ariaPrefix={`Edit ${label.name}`}
                  />
                  <button
                    type="button"
                    onClick={onSaveEdit}
                    style={styles.primaryButton}
                  >
                    Save
                  </button>
                  <button
                    type="button"
                    onClick={() => setEditingId(null)}
                    style={styles.secondaryButton}
                  >
                    Cancel
                  </button>
                </>
              ) : (
                <>
                  <span style={styles.chip}>
                    <span
                      style={{ ...styles.dot, background: label.color }}
                      aria-hidden="true"
                    />
                    {label.name}
                  </span>
                  <span style={styles.spacer} />
                  <button
                    type="button"
                    onClick={() => startEdit(label)}
                    style={styles.secondaryButton}
                  >
                    Edit
                  </button>
                  <button
                    type="button"
                    onClick={() => onDelete(label.id)}
                    style={styles.dangerButton}
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
    <span style={styles.colorRow}>
      {LABEL_COLORS.map((c) => (
        <button
          key={c}
          type="button"
          onClick={() => onChange(c)}
          aria-label={`${ariaPrefix} colour ${c}`}
          aria-pressed={value === c}
          title={c}
          style={{
            ...styles.swatch,
            background: c,
            outline: value === c ? "2px solid #111827" : "none",
          }}
        />
      ))}
    </span>
  );
}

const styles: Record<string, React.CSSProperties> = {
  root: { padding: "1rem", maxWidth: "720px" },
  header: {
    display: "flex",
    alignItems: "baseline",
    justifyContent: "space-between",
    marginBottom: "1rem",
  },
  title: { margin: 0, fontSize: "1.25rem" },
  backLink: { color: "#2563eb", textDecoration: "none", fontSize: "0.9rem" },
  createRow: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    flexWrap: "wrap",
    marginBottom: "1rem",
  },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  colorRow: { display: "inline-flex", gap: "0.2rem", flexWrap: "wrap" },
  swatch: {
    width: "1.25rem",
    height: "1.25rem",
    borderRadius: "0.25rem",
    border: "1px solid rgba(0,0,0,0.1)",
    cursor: "pointer",
    padding: 0,
  },
  list: { listStyle: "none", margin: 0, padding: 0, display: "grid", gap: "0.4rem" },
  listItem: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    padding: "0.4rem 0.5rem",
    border: "1px solid #e5e7eb",
    borderRadius: "0.375rem",
    background: "#fff",
  },
  chip: {
    display: "inline-flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.9rem",
    fontWeight: 600,
    color: "#111827",
  },
  dot: {
    width: "0.85rem",
    height: "0.85rem",
    borderRadius: "999px",
    display: "inline-block",
  },
  spacer: { flex: 1 },
  primaryButton: {
    padding: "0.4rem 0.8rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  secondaryButton: {
    padding: "0.4rem 0.8rem",
    background: "#fff",
    color: "#374151",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  dangerButton: {
    padding: "0.4rem 0.8rem",
    background: "#fff",
    color: "#991b1b",
    border: "1px solid #fca5a5",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  muted: { color: "#6b7280", fontStyle: "italic", fontSize: "0.85rem" },
};
