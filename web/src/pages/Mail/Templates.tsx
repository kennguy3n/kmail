import { useState } from "react";
import { Link } from "react-router-dom";

import { cn } from "../../lib/cn";

import {
  createTemplate,
  deleteTemplate,
  extractVariables,
  listTemplates,
  updateTemplate,
} from "../../api/templates";
import type { EmailTemplate, EmailTemplateDraft } from "../../types";
import RichTextEditor from "./RichTextEditor";

/**
 * Template management page.
 *
 * CRUD over the {@link listTemplates} store. Bodies are HTML edited
 * in the shared {@link RichTextEditor} and may contain
 * `{{variable}}` placeholders (built-ins: `sender_name`, `company`,
 * `date`); the Compose template picker expands them at insert time.
 * This page surfaces the placeholders it detects so the author can
 * see what the picker will prompt for.
 */
const emptyDraft = (): EmailTemplateDraft => ({
  name: "",
  subject: "",
  body: "",
  scope: "personal",
});

export default function Templates() {
  const [templates, setTemplates] = useState<EmailTemplate[]>(() =>
    listTemplates(),
  );
  const [editingId, setEditingId] = useState<string | null>(null);
  const [draft, setDraft] = useState<EmailTemplateDraft>(emptyDraft);
  const [info, setInfo] = useState<string | null>(null);

  const refresh = () => setTemplates(listTemplates());
  const variables = extractVariables(draft.subject, draft.body);

  const startNew = () => {
    setEditingId(null);
    setDraft(emptyDraft());
    setInfo(null);
  };

  const startEdit = (tpl: EmailTemplate) => {
    setEditingId(tpl.id);
    setDraft({
      name: tpl.name,
      subject: tpl.subject,
      body: tpl.body,
      scope: tpl.scope,
    });
    setInfo(null);
  };

  const onSave = () => {
    if (editingId) {
      updateTemplate(editingId, draft);
      setInfo("Template updated.");
    } else {
      const created = createTemplate(draft);
      setEditingId(created.id);
      setInfo("Template created.");
    }
    refresh();
  };

  const onDelete = (id: string) => {
    deleteTemplate(id);
    if (editingId === id) startNew();
    refresh();
  };

  return (
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Templates</h2>
        <Link to="/mail" className={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div className={styles.layout}>
        <aside className={styles.list}>
          <button type="button" onClick={startNew} className={styles.newButton}>
            + New template
          </button>
          {templates.length === 0 && (
            <p className={styles.muted}>No templates yet.</p>
          )}
          <ul className={styles.ul}>
            {templates.map((t) => (
              <li key={t.id}>
                <button
                  type="button"
                  onClick={() => startEdit(t)}
                  className={cn(
                    styles.listItem,
                    editingId === t.id && styles.listItemActive,
                  )}
                >
                  <span className={styles.listItemName}>{t.name}</span>
                  <span className={styles.listItemScope}>
                    {t.scope === "shared" ? "Shared" : "Personal"}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div className={styles.editor}>
          {info && <div className={styles.info}>{info}</div>}
          <label className={styles.fieldLabel} htmlFor="tpl-name">
            Name
          </label>
          <input
            id="tpl-name"
            type="text"
            value={draft.name}
            onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
            placeholder="e.g. Meeting follow-up"
            className={styles.input}
          />

          <label className={styles.fieldLabel} htmlFor="tpl-subject">
            Subject
          </label>
          <input
            id="tpl-subject"
            type="text"
            value={draft.subject}
            onChange={(e) =>
              setDraft((d) => ({ ...d, subject: e.target.value }))
            }
            placeholder="Hi {{sender_name}}…"
            className={styles.input}
          />

          <label className={styles.fieldLabel} htmlFor="tpl-scope">
            Scope
          </label>
          <select
            id="tpl-scope"
            value={draft.scope}
            onChange={(e) =>
              setDraft((d) => ({
                ...d,
                scope: e.target.value as EmailTemplateDraft["scope"],
              }))
            }
            className={styles.input}
          >
            <option value="personal">Personal</option>
            <option value="shared">Shared (tenant)</option>
          </select>

          <label className={styles.fieldLabel}>Body</label>
          <RichTextEditor
            value={draft.body}
            onChange={(body) => setDraft((d) => ({ ...d, body }))}
            ariaLabel="Template body"
            placeholder="Write your template. Use {{variable}} placeholders…"
            minHeight={160}
          />

          <p className={styles.varsHint}>
            Variables detected:{" "}
            {variables.length > 0 ? (
              variables.map((v) => (
                <code key={v} className={styles.varChip}>{`{{${v}}}`}</code>
              ))
            ) : (
              <span className={styles.muted}>none</span>
            )}
            <br />
            Built-ins: <code className={styles.varChip}>{"{{sender_name}}"}</code>
            <code className={styles.varChip}>{"{{company}}"}</code>
            <code className={styles.varChip}>{"{{date}}"}</code>
          </p>

          <div className={styles.buttonRow}>
            <button type="button" onClick={onSave} className={styles.primaryButton}>
              {editingId ? "Save changes" : "Create template"}
            </button>
            {editingId && (
              <button
                type="button"
                onClick={() => onDelete(editingId)}
                className={styles.dangerButton}
              >
                Delete
              </button>
            )}
          </div>
        </div>
      </div>
    </section>
  );
}

/** Theme-aware Tailwind class recipes for the Templates manager. */
const styles: Record<string, string> = {
  root: "max-w-[1000px] p-4",
  header: "mb-4 flex items-baseline justify-between",
  title: "m-0 text-xl font-semibold",
  backLink: "text-sm text-primary no-underline hover:underline",
  layout: "grid grid-cols-[240px_1fr] gap-4",
  list: "border-r border-border pr-4",
  newButton:
    "mb-2 w-full cursor-pointer rounded-md border-0 bg-primary p-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  ul: "m-0 grid list-none gap-1 p-0",
  listItem:
    "flex w-full cursor-pointer flex-col gap-0.5 rounded-md border border-border bg-surface px-2 py-1.5 text-left transition-colors hover:bg-surface-hover",
  listItemActive: "border-primary bg-primary-subtle",
  listItemName: "text-sm font-semibold text-fg",
  listItemScope: "text-xs text-fg-muted",
  editor: "grid content-start gap-2",
  fieldLabel: "text-xs font-semibold text-fg-muted",
  input:
    "rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  varsHint: "m-0 text-xs leading-loose text-fg-muted",
  varChip:
    "mx-1 rounded-sm border border-border bg-surface-muted px-1 py-px text-xs",
  buttonRow: "mt-1 flex gap-2",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3.5 py-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  dangerButton:
    "cursor-pointer rounded-md border border-danger/40 bg-surface px-3.5 py-1.5 text-sm text-danger-fg transition-colors hover:bg-danger-bg",
  info: "rounded-md bg-success-bg px-2.5 py-1.5 text-sm text-success-fg",
  muted: "text-sm italic text-fg-muted",
};
