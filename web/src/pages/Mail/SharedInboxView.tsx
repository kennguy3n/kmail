import { useEffect, useState } from "react";

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
  const [inboxId, setInboxId] = useState("");
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

      <table className="kmail-admin-table">
        <thead>
          <tr>
            <th>Email</th>
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
              <td>{r.email_id}</td>
              <td>{r.assignee_user_id || "—"}</td>
              <td>{r.status}</td>
              <td>{new Date(r.updated_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>

      {selected && (
        <div className="kmail-inbox-detail">
          <h3>{selected.email_id}</h3>
          <div className="kmail-inbox-controls">
            <label>
              Assign to
              <input value={assignee} onChange={(e) => setAssignee(e.target.value)} />
              <button type="button" onClick={doAssign}>Assign</button>
            </label>
            <label>
              Status
              <select value={selected.status} onChange={(e) => doStatus(e.target.value as AssignmentStatus)}>
                {STATUS_OPTIONS.map((s) => (
                  <option key={s} value={s}>{s}</option>
                ))}
              </select>
            </label>
          </div>

          <h4>Internal notes</h4>
          <ul>
            {notes.map((n) => (
              <li key={n.id}>
                <strong>{n.author_user_id}</strong>{" "}
                <span>{new Date(n.created_at).toLocaleString()}</span>
                <p>{n.note_text}</p>
              </li>
            ))}
          </ul>
          <textarea value={noteText} onChange={(e) => setNoteText(e.target.value)} />
          <button type="button" onClick={doNote}>Add note</button>
        </div>
      )}
    </section>
  );
}
