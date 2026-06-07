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
    <div className={styles.overlay} role="dialog" aria-modal="true" aria-label="Insert template">
      <div className={styles.modal}>
        <header className={styles.header}>
          <h3 className={styles.title}>Insert template</h3>
          <button
            type="button"
            onClick={onClose}
            className={styles.close}
            aria-label="Close"
          >
            ×
          </button>
        </header>

        {templates.length === 0 ? (
          <p className={styles.muted}>
            No templates yet. Create one on the Templates page.
          </p>
        ) : (
          <>
            <label className={styles.fieldLabel} htmlFor="tpl-pick">
              Template
            </label>
            <select
              id="tpl-pick"
              value={selectedId}
              onChange={(e) => {
                setSelectedId(e.target.value);
                setValues({});
              }}
              className={styles.input}
            >
              {templates.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name}
                </option>
              ))}
            </select>

            {customVars.length > 0 && (
              <div className={styles.vars}>
                <p className={styles.varsHint}>Fill in template values:</p>
                {customVars.map((v) => (
                  <div key={v} className={styles.varRow}>
                    <label className={styles.fieldLabel} htmlFor={`var-${v}`}>
                      {v}
                    </label>
                    <input
                      id={`var-${v}`}
                      type="text"
                      value={values[v] ?? ""}
                      onChange={(e) =>
                        setValues((cur) => ({ ...cur, [v]: e.target.value }))
                      }
                      className={styles.input}
                    />
                  </div>
                ))}
              </div>
            )}

            <div className={styles.buttonRow}>
              <button
                type="button"
                onClick={apply}
                disabled={!selected}
                className={styles.primaryButton}
              >
                Insert
              </button>
              <button
                type="button"
                onClick={onClose}
                className={styles.secondaryButton}
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

/** Theme-aware Tailwind class recipes for the TemplatePicker modal. */
const styles: Record<string, string> = {
  overlay:
    "fixed inset-0 z-modal flex items-center justify-center bg-overlay",
  modal:
    "grid w-[min(480px,92vw)] gap-2 rounded-lg bg-elevated p-4 shadow-lg",
  header: "flex items-center justify-between",
  title: "m-0 text-base font-semibold",
  close:
    "cursor-pointer border-0 bg-transparent text-2xl leading-none text-fg-muted hover:text-fg",
  fieldLabel: "text-xs font-semibold text-fg-muted",
  input:
    "box-border w-full rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  vars: "grid gap-1.5 rounded-md bg-surface-muted p-2",
  varsHint: "m-0 text-xs text-fg-muted",
  varRow: "grid gap-1",
  buttonRow: "mt-1 flex gap-2",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3.5 py-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  secondaryButton:
    "cursor-pointer rounded-md border border-border bg-surface px-3.5 py-1.5 text-sm text-fg transition-colors hover:bg-surface-hover",
  muted: "text-sm italic text-fg-muted",
};
