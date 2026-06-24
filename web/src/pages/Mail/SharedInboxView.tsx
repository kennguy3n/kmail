import { useEffect, useState } from "react";

import { cn } from "../../lib/cn";
import {
  addNote,
  assignEmail,
  listAssignments,
  listNotes,
  setStatus,
  type AssignmentStatus,
  type EmailAssignment,
  type InternalNote,
} from "../../api/sharedinbox";

const STATUS_OPTIONS: AssignmentStatus[] = [
  "open",
  "in_progress",
  "waiting",
  "resolved",
  "closed",
];

/**
 * SharedInboxView renders the shared-inbox workflow overlay:
 * assignment list, status selector, assign-to control, and an
 * internal-notes panel visible only to shared inbox members.
 */
export default function SharedInboxView() {
  const [inboxId, setInboxId] = useState("shared-support");
  const [filter, setFilter] = useState<AssignmentStatus | "">("");
  const [rows, setRows] = useState<EmailAssignment[]>([]);
  const [selected, setSelected] = useState<EmailAssignment | null>(null);
  const [notes, setNotes] = useState<InternalNote[]>([]);
  const [noteText, setNoteText] = useState("");
  const [assignee, setAssignee] = useState("");
  const [error, setError] = useState<string | null>(null);

  const reload = async () => {
    if (!inboxId) return;
    try {
      const r = await listAssignments(inboxId, filter ? { status: filter } : {});
      setRows(r);
    } catch (e) {
      setError(String(e));
    }
  };

  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inboxId, filter]);

  useEffect(() => {
    if (!selected) {
      setNotes([]);
      return;
    }
    listNotes(inboxId, selected.email_id).then(setNotes).catch((e) => setError(String(e)));
  }, [selected, inboxId]);

  const doAssign = async () => {
    if (!selected || !assignee) return;
    try {
      const out = await assignEmail(inboxId, selected.email_id, assignee);
      setSelected(out);
      await reload();
    } catch (e) {
      setError(String(e));
    }
  };

  const doStatus = async (status: AssignmentStatus) => {
    if (!selected) return;
    try {
      const out = await setStatus(inboxId, selected.email_id, status);
      setSelected(out);
      await reload();
    } catch (e) {
      setError(String(e));
    }
  };

  const doNote = async () => {
    if (!selected || !noteText.trim()) return;
    try {
      await addNote(inboxId, selected.email_id, noteText.trim());
      setNoteText("");
      const fresh = await listNotes(inboxId, selected.email_id);
      setNotes(fresh);
    } catch (e) {
      setError(String(e));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Shared inbox workflows</h2>
      {error && <p className="kmail-error">{error}</p>}

      <div className="kmail-admin-controls">
        <label>
          Shared inbox ID
          <input value={inboxId} onChange={(e) => setInboxId(e.target.value)} />
        </label>
        <label>
          Status
          <select value={filter} onChange={(e) => setFilter(e.target.value as AssignmentStatus)}>
            <option value="">(all)</option>
            {STATUS_OPTIONS.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </label>
      </div>

      {rows.length === 0 && !error && (
        <p className="kmail-admin-hint">No assignments found for this inbox.</p>
      )}
      {rows.length > 0 && (
        <table className="kmail-admin-table">
          <thead>
            <tr>
              <th>Conversation</th>
              <th>Assignee</th>
              <th>Status</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr
                key={r.id}
                onClick={() => setSelected(r)}
                className={selected?.id === r.id ? "selected" : ""}
              >
                <td>
                  <div className="font-semibold text-fg">
                    {r.subject || r.email_id}
                  </div>
                  <div className="text-xs text-fg-muted">
                    {r.sender_name ? `${r.sender_name} <${r.sender_email}>` : r.sender_email}
                    {r.preview && ` — ${r.preview}`}
                  </div>
                </td>
                <td>{r.assignee_user_id || "—"}</td>
                <td>
                  <span className={cn(
                    "inline-flex rounded-pill px-2.5 py-0.5 text-xs font-semibold",
                    r.status === "open" && "bg-info-bg text-info-fg",
                    r.status === "in_progress" && "bg-warning-bg text-warning-fg",
                    r.status === "waiting" && "bg-warning-bg text-warning-fg",
                    r.status === "resolved" && "bg-success-bg text-success-fg",
                    r.status === "closed" && "bg-surface-muted text-fg-muted",
                  )}>
                    {r.status.replace("_", " ")}
                  </span>
                </td>
                <td>{new Date(r.updated_at).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {selected && (
        <div className="kmail-inbox-detail">
          <h3>{selected.subject || selected.email_id}</h3>
          <div className="kmail-admin-hint">
            {selected.sender_name ? `${selected.sender_name} <${selected.sender_email}>` : selected.sender_email}
          </div>
          <div className="kmail-inbox-controls">
            <label>
              Assign to
              <input value={assignee} onChange={(e) => setAssignee(e.target.value)} />
            </label>
            <button type="button" className="kmail-button" onClick={doAssign}>Assign</button>
            <label>
              Status
              <select
                value={selected.status}
                onChange={(e) => doStatus(e.target.value as AssignmentStatus)}
              >
                {STATUS_OPTIONS.map((s) => (
                  <option key={s} value={s}>{s}</option>
                ))}
              </select>
            </label>
          </div>

          <h4>Internal notes</h4>
          {notes.length === 0 && <p className="kmail-admin-hint">No notes yet.</p>}
          <ul>
            {notes.map((n) => (
              <li key={n.id}>
                <strong>{n.author_user_id}</strong>{" "}
                <span>{new Date(n.created_at).toLocaleString()}</span>
                <p>{n.note_text}</p>
              </li>
            ))}
          </ul>
          <textarea
            aria-label="Internal note"
            value={noteText}
            onChange={(e) => setNoteText(e.target.value)}
          />
          <button type="button" className="kmail-button" onClick={doNote}>Add note</button>
        </div>
      )}
    </section>
  );
}
