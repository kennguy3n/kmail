import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import {
  createSignature,
  deleteSignature,
  listSignatures,
  updateSignature,
} from "../../api/signatures";
import type { Identity, Signature, SignatureDraft } from "../../types";
import RichTextEditor from "./RichTextEditor";

/**
 * Signature management page.
 *
 * CRUD over the client-side {@link listSignatures} store: a user can
 * keep several signatures (personal, work, …), edit each in the
 * shared {@link RichTextEditor}, scope one to a specific From
 * identity, and mark a default per scope. Compose reads the default
 * via `defaultSignatureFor` when auto-appending.
 */
const emptyDraft = (): SignatureDraft => ({
  name: "",
  html: "",
  identityEmail: null,
  isDefault: false,
});

export default function SignatureEditor() {
  const [signatures, setSignatures] = useState<Signature[]>(() =>
    listSignatures(),
  );
  const [identities, setIdentities] = useState<Identity[]>([]);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [draft, setDraft] = useState<SignatureDraft>(emptyDraft);
  const [info, setInfo] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    jmapClient
      .getIdentities()
      .then((list) => {
        if (!cancelled) setIdentities(list);
      })
      .catch(() => {
        // Identities are a convenience for scoping; the editor still
        // works with the "any identity" scope if the fetch fails.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const refresh = () => setSignatures(listSignatures());

  const startNew = () => {
    setEditingId(null);
    setDraft(emptyDraft());
    setInfo(null);
  };

  const startEdit = (sig: Signature) => {
    setEditingId(sig.id);
    setDraft({
      name: sig.name,
      html: sig.html,
      identityEmail: sig.identityEmail,
      isDefault: sig.isDefault,
    });
    setInfo(null);
  };

  const onSave = () => {
    if (editingId) {
      updateSignature(editingId, draft);
      setInfo("Signature updated.");
    } else {
      const created = createSignature(draft);
      setEditingId(created.id);
      setInfo("Signature created.");
    }
    refresh();
  };

  const onDelete = (id: string) => {
    deleteSignature(id);
    if (editingId === id) startNew();
    refresh();
  };

  const scopeLabel = useMemo(
    () => (s: Signature) =>
      s.identityEmail ? s.identityEmail : "Any identity",
    [],
  );

  return (
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Signatures</h2>
        <Link to="/mail" style={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div style={styles.layout}>
        <aside style={styles.list}>
          <button type="button" onClick={startNew} style={styles.newButton}>
            + New signature
          </button>
          {signatures.length === 0 && (
            <p style={styles.muted}>No signatures yet.</p>
          )}
          <ul style={styles.ul}>
            {signatures.map((s) => (
              <li key={s.id}>
                <button
                  type="button"
                  onClick={() => startEdit(s)}
                  style={{
                    ...styles.listItem,
                    ...(editingId === s.id ? styles.listItemActive : {}),
                  }}
                >
                  <span style={styles.listItemName}>
                    {s.name}
                    {s.isDefault && <span style={styles.defaultBadge}>default</span>}
                  </span>
                  <span style={styles.listItemScope}>{scopeLabel(s)}</span>
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div style={styles.editor}>
          {info && <div style={styles.info}>{info}</div>}
          <label style={styles.fieldLabel} htmlFor="sig-name">
            Name
          </label>
          <input
            id="sig-name"
            type="text"
            value={draft.name}
            onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
            placeholder="e.g. Work signature"
            style={styles.input}
          />

          <label style={styles.fieldLabel} htmlFor="sig-identity">
            Scope (From identity)
          </label>
          <select
            id="sig-identity"
            value={draft.identityEmail ?? ""}
            onChange={(e) =>
              setDraft((d) => ({
                ...d,
                identityEmail: e.target.value === "" ? null : e.target.value,
              }))
            }
            style={styles.input}
          >
            <option value="">Any identity</option>
            {identities.map((id) => (
              <option key={id.id} value={id.email}>
                {id.name ? `${id.name} <${id.email}>` : id.email}
              </option>
            ))}
          </select>

          <label style={styles.fieldLabel}>Content</label>
          <RichTextEditor
            value={draft.html}
            onChange={(html) => setDraft((d) => ({ ...d, html }))}
            ariaLabel="Signature content"
            placeholder="Type your signature…"
            minHeight={140}
          />

          <label style={styles.checkboxRow}>
            <input
              type="checkbox"
              checked={draft.isDefault}
              onChange={(e) =>
                setDraft((d) => ({ ...d, isDefault: e.target.checked }))
              }
            />
            Default for this scope (auto-appended on compose/reply/forward)
          </label>

          <div style={styles.buttonRow}>
            <button type="button" onClick={onSave} style={styles.primaryButton}>
              {editingId ? "Save changes" : "Create signature"}
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
  listItemName: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.9rem",
    fontWeight: 600,
    color: "#111827",
  },
  listItemScope: { fontSize: "0.75rem", color: "#6b7280" },
  defaultBadge: {
    fontSize: "0.65rem",
    fontWeight: 700,
    color: "#1d4ed8",
    background: "#dbeafe",
    padding: "0.05rem 0.3rem",
    borderRadius: "999px",
  },
  editor: { display: "grid", gap: "0.5rem", alignContent: "start" },
  fieldLabel: { fontSize: "0.8rem", fontWeight: 600, color: "#374151" },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  checkboxRow: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.85rem",
    color: "#374151",
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
