import { useState } from "react";
import { Link } from "react-router-dom";

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
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Templates</h2>
        <Link to="/mail" style={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div style={styles.layout}>
        <aside style={styles.list}>
          <button type="button" onClick={startNew} style={styles.newButton}>
            + New template
          </button>
          {templates.length === 0 && (
            <p style={styles.muted}>No templates yet.</p>
          )}
          <ul style={styles.ul}>
            {templates.map((t) => (
              <li key={t.id}>
                <button
                  type="button"
                  onClick={() => startEdit(t)}
                  style={{
                    ...styles.listItem,
                    ...(editingId === t.id ? styles.listItemActive : {}),
                  }}
                >
                  <span style={styles.listItemName}>{t.name}</span>
                  <span style={styles.listItemScope}>
                    {t.scope === "shared" ? "Shared" : "Personal"}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div style={styles.editor}>
          {info && <div style={styles.info}>{info}</div>}
          <label style={styles.fieldLabel} htmlFor="tpl-name">
            Name
          </label>
          <input
            id="tpl-name"
            type="text"
            value={draft.name}
            onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
            placeholder="e.g. Meeting follow-up"
            style={styles.input}
          />

          <label style={styles.fieldLabel} htmlFor="tpl-subject">
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
            style={styles.input}
          />

          <label style={styles.fieldLabel} htmlFor="tpl-scope">
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
            style={styles.input}
          >
            <option value="personal">Personal</option>
            <option value="shared">Shared (tenant)</option>
          </select>

          <label style={styles.fieldLabel}>Body</label>
          <RichTextEditor
            value={draft.body}
            onChange={(body) => setDraft((d) => ({ ...d, body }))}
            ariaLabel="Template body"
            placeholder="Write your template. Use {{variable}} placeholders…"
            minHeight={160}
          />

          <p style={styles.varsHint}>
            Variables detected:{" "}
            {variables.length > 0 ? (
              variables.map((v) => (
                <code key={v} style={styles.varChip}>{`{{${v}}}`}</code>
              ))
            ) : (
              <span style={styles.muted}>none</span>
            )}
            <br />
            Built-ins: <code style={styles.varChip}>{"{{sender_name}}"}</code>
            <code style={styles.varChip}>{"{{company}}"}</code>
            <code style={styles.varChip}>{"{{date}}"}</code>
          </p>

          <div style={styles.buttonRow}>
            <button type="button" onClick={onSave} style={styles.primaryButton}>
              {editingId ? "Save changes" : "Create template"}
            </button>
            {editingId && (
              <button
                type="button"
                onClick={() => onDelete(editingId)}
                style={styles.dangerButton}
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

const styles: Record<string, React.CSSProperties> = {
  root: { padding: "1rem", maxWidth: "1000px" },
  header: {
    display: "flex",
    alignItems: "baseline",
    justifyContent: "space-between",
    marginBottom: "1rem",
  },
  title: { margin: 0, fontSize: "1.25rem" },
  backLink: { color: "#2563eb", textDecoration: "none", fontSize: "0.9rem" },
  layout: { display: "grid", gridTemplateColumns: "240px 1fr", gap: "1rem" },
  list: { borderRight: "1px solid #e5e7eb", paddingRight: "1rem" },
  newButton: {
    width: "100%",
    padding: "0.4rem",
    marginBottom: "0.5rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  ul: { listStyle: "none", margin: 0, padding: 0, display: "grid", gap: "0.25rem" },
  listItem: {
    display: "flex",
    flexDirection: "column",
    gap: "0.15rem",
    width: "100%",
    padding: "0.4rem 0.5rem",
    background: "#fff",
    border: "1px solid #e5e7eb",
    borderRadius: "0.25rem",
    cursor: "pointer",
    textAlign: "left",
  },
  listItemActive: { background: "#eff6ff", borderColor: "#2563eb" },
  listItemName: { fontSize: "0.9rem", fontWeight: 600, color: "#111827" },
  listItemScope: { fontSize: "0.75rem", color: "#6b7280" },
  editor: { display: "grid", gap: "0.5rem", alignContent: "start" },
  fieldLabel: { fontSize: "0.8rem", fontWeight: 600, color: "#374151" },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  varsHint: { fontSize: "0.8rem", color: "#374151", margin: 0, lineHeight: 1.9 },
  varChip: {
    background: "#f3f4f6",
    border: "1px solid #e5e7eb",
    borderRadius: "0.25rem",
    padding: "0.05rem 0.3rem",
    margin: "0 0.2rem",
    fontSize: "0.75rem",
  },
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
  dangerButton: {
    padding: "0.45rem 0.9rem",
    background: "#fff",
    color: "#991b1b",
    border: "1px solid #fca5a5",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  info: {
    padding: "0.4rem 0.6rem",
    background: "#ecfdf5",
    color: "#065f46",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
  },
  muted: { color: "#6b7280", fontStyle: "italic", fontSize: "0.85rem" },
};
