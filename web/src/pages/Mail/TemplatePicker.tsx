import { useMemo, useState } from "react";

import {
  builtinVariables,
  extractVariables,
  renderTemplate,
} from "../../api/templates";
import type { EmailTemplate } from "../../types";

/**
 * Modal for inserting an email template into Compose.
 *
 * The user picks a template, fills any `{{variable}}` placeholders
 * the built-ins (`sender_name`, `company`, `date`) don't cover, and
 * applies it. Subject + body are rendered through
 * {@link renderTemplate} and handed back to Compose, which decides
 * whether to drop the HTML into the rich editor or its plain-text
 * equivalent.
 */
export interface TemplatePickerProps {
  templates: EmailTemplate[];
  /** Pre-fills the `sender_name` built-in from the active identity. */
  senderName: string;
  onApply: (result: { subject: string; body: string }) => void;
  onClose: () => void;
}

export default function TemplatePicker({
  templates,
  senderName,
  onApply,
  onClose,
}: TemplatePickerProps) {
  const [selectedId, setSelectedId] = useState<string>(
    templates[0]?.id ?? "",
  );
  const [values, setValues] = useState<Record<string, string>>({});

  const selected = useMemo(
    () => templates.find((t) => t.id === selectedId) ?? null,
    [templates, selectedId],
  );

  // Variables the author referenced that the built-ins don't cover,
  // so we only prompt for what actually needs a value.
  const customVars = useMemo(() => {
    if (!selected) return [];
    const builtins = new Set(Object.keys(builtinVariables()));
    return extractVariables(selected.subject, selected.body).filter(
      (v) => !builtins.has(v),
    );
  }, [selected]);

  const apply = () => {
    if (!selected) return;
    const merged = builtinVariables({ sender_name: senderName, ...values });
    onApply({
      subject: renderTemplate(selected.subject, merged),
      body: renderTemplate(selected.body, merged),
    });
  };

  return (
    <div style={styles.overlay} role="dialog" aria-modal="true" aria-label="Insert template">
      <div style={styles.modal}>
        <header style={styles.header}>
          <h3 style={styles.title}>Insert template</h3>
          <button
            type="button"
            onClick={onClose}
            style={styles.close}
            aria-label="Close"
          >
            ×
          </button>
        </header>

        {templates.length === 0 ? (
          <p style={styles.muted}>
            No templates yet. Create one on the Templates page.
          </p>
        ) : (
          <>
            <label style={styles.fieldLabel} htmlFor="tpl-pick">
              Template
            </label>
            <select
              id="tpl-pick"
              value={selectedId}
              onChange={(e) => {
                setSelectedId(e.target.value);
                setValues({});
              }}
              style={styles.input}
            >
              {templates.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </select>

            {customVars.length > 0 && (
              <div style={styles.vars}>
                <p style={styles.varsHint}>Fill in template values:</p>
                {customVars.map((v) => (
                  <div key={v} style={styles.varRow}>
                    <label style={styles.fieldLabel} htmlFor={`var-${v}`}>
                      {v}
                    </label>
                    <input
                      id={`var-${v}`}
                      type="text"
                      value={values[v] ?? ""}
                      onChange={(e) =>
                        setValues((cur) => ({ ...cur, [v]: e.target.value }))
                      }
                      style={styles.input}
                    />
                  </div>
                ))}
              </div>
            )}

            <div style={styles.buttonRow}>
              <button
                type="button"
                onClick={apply}
                disabled={!selected}
                style={styles.primaryButton}
              >
                Insert
              </button>
              <button
                type="button"
                onClick={onClose}
                style={styles.secondaryButton}
              >
                Cancel
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

const styles: Record<string, React.CSSProperties> = {
  overlay: {
    position: "fixed",
    inset: 0,
    background: "rgba(0,0,0,0.4)",
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    zIndex: 50,
  },
  modal: {
    background: "#fff",
    borderRadius: "0.5rem",
    padding: "1rem",
    width: "min(480px, 92vw)",
    display: "grid",
    gap: "0.5rem",
    boxShadow: "0 10px 40px rgba(0,0,0,0.2)",
  },
  header: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
  },
  title: { margin: 0, fontSize: "1.05rem" },
  close: {
    border: "none",
    background: "none",
    fontSize: "1.4rem",
    lineHeight: 1,
    cursor: "pointer",
    color: "#6b7280",
  },
  fieldLabel: { fontSize: "0.8rem", fontWeight: 600, color: "#374151" },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    width: "100%",
    boxSizing: "border-box",
  },
  vars: {
    display: "grid",
    gap: "0.4rem",
    padding: "0.5rem",
    background: "#f9fafb",
    borderRadius: "0.375rem",
  },
  varsHint: { margin: 0, fontSize: "0.8rem", color: "#374151" },
  varRow: { display: "grid", gap: "0.2rem" },
  buttonRow: { display: "flex", gap: "0.5rem", marginTop: "0.25rem" },
  primaryButton: {
    padding: "0.45rem 0.9rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  secondaryButton: {
    padding: "0.45rem 0.9rem",
    background: "#fff",
    color: "#374151",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  muted: { color: "#6b7280", fontStyle: "italic", fontSize: "0.85rem" },
};
