import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { PenLine } from "lucide-react";

import { cn } from "../../lib/cn";
import { Button } from "../../components/ui/Button";
import { EmptyState } from "../../components/ui/EmptyState";

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
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Signatures</h2>
        <Link to="/mail" className={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div className={styles.layout}>
        <aside className={styles.list}>
          <button type="button" onClick={startNew} className={styles.newButton}>
            + New signature
          </button>
          {signatures.length === 0 && (
            <EmptyState
              icon={<PenLine />}
              title="No signatures yet"
              description="Create a signature to automatically append it to your emails."
              action={
                <Button onClick={startNew}>Create signature</Button>
              }
            />
          )}
          <ul className={styles.ul}>
            {signatures.map((s) => (
              <li key={s.id}>
                <button
                  type="button"
                  onClick={() => startEdit(s)}
                  className={cn(
                    styles.listItem,
                    editingId === s.id && styles.listItemActive,
                  )}
                >
                  <span className={styles.listItemName}>
                    {s.name}
                    {s.isDefault && <span className={styles.defaultBadge}>default</span>}
                  </span>
                  <span className={styles.listItemScope}>{scopeLabel(s)}</span>
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div className={styles.editor}>
          {info && <div className={styles.info}>{info}</div>}
          <label className={styles.fieldLabel} htmlFor="sig-name">
            Name
          </label>
          <input
            id="sig-name"
            type="text"
            value={draft.name}
            onChange={(e) => setDraft((d) => ({ ...d, name: e.target.value }))}
            placeholder="e.g. Work signature"
            className={styles.input}
          />

          <label className={styles.fieldLabel} htmlFor="sig-identity">
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
            className={styles.input}
          >
            <option value="">Any identity</option>
            {identities.map((id) => (
              <option key={id.id} value={id.email}>
                {id.name ? `${id.name} <${id.email}>` : id.email}
              </option>
            ))}
          </select>

          <label className={styles.fieldLabel}>Content</label>
          <RichTextEditor
            value={draft.html}
            onChange={(html) => setDraft((d) => ({ ...d, html }))}
            ariaLabel="Signature content"
            placeholder="Type your signature…"
            minHeight={140}
          />

          <label className={styles.checkboxRow}>
            <input
              type="checkbox"
              checked={draft.isDefault}
              onChange={(e) =>
                setDraft((d) => ({ ...d, isDefault: e.target.checked }))
              }
            />
            Default for this scope (auto-appended on compose/reply/forward)
          </label>

          <div className={styles.buttonRow}>
            <button type="button" onClick={onSave} className={styles.primaryButton}>
              {editingId ? "Save changes" : "Create signature"}
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

/** Theme-aware Tailwind class recipes for the Signature editor. */
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
  listItemName: "flex items-center gap-1.5 text-sm font-semibold text-fg",
  listItemScope: "text-xs text-fg-muted",
  defaultBadge:
    "rounded-pill bg-primary-subtle px-1 py-px text-[0.65rem] font-bold text-primary",
  editor: "grid content-start gap-2",
  fieldLabel: "text-xs font-semibold text-fg-muted",
  input:
    "rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  checkboxRow: "flex items-center gap-1.5 text-sm text-fg-muted",
  buttonRow: "mt-1 flex gap-2",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3.5 py-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  dangerButton:
    "cursor-pointer rounded-md border border-danger/40 bg-surface px-3.5 py-1.5 text-sm text-danger-fg transition-colors hover:bg-danger-bg",
  info: "rounded-md bg-success-bg px-2.5 py-1.5 text-sm text-success-fg",
  muted: "text-sm italic text-fg-muted",
};
