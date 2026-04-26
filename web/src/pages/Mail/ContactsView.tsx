/**
 * Contacts view — lists CardDAV address books, contacts, and
 * supports basic CRUD from the BFF.
 */

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  createContact,
  deleteContact,
  listAddressBooks,
  listContacts,
  updateContact,
} from "../../api/contacts";
import { useTenantSelection } from "../Admin/useTenantSelection";
import type { AddressBook, Contact, ContactDraft } from "../../types";

const emptyDraft: ContactDraft = { fn: "", emails: [], phones: [], org: "", note: "" };

export default function ContactsView() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [accountId, setAccountId] = useState("");
  const [books, setBooks] = useState<AddressBook[]>([]);
  const [bookId, setBookId] = useState<string>("");
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [search, setSearch] = useState("");
  const [editing, setEditing] = useState<Contact | null>(null);
  const [draft, setDraft] = useState<ContactDraft>(emptyDraft);
  const [error, setError] = useState<string | null>(null);

  const loadBooks = useCallback(() => {
    if (!selectedTenantId || !accountId) return;
    listAddressBooks(selectedTenantId, accountId)
      .then((b) => {
        setBooks(b);
        setBookId(b.find((x) => x.isDefault)?.id ?? b[0]?.id ?? "");
      })
      .catch((e: unknown) => setError(String(e)));
  }, [selectedTenantId, accountId]);

  const loadContacts = useCallback(() => {
    if (!selectedTenantId || !accountId || !bookId) return;
    listContacts(selectedTenantId, accountId, bookId)
      .then(setContacts)
      .catch((e: unknown) => setError(String(e)));
  }, [selectedTenantId, accountId, bookId]);

  useEffect(loadBooks, [loadBooks]);
  useEffect(loadContacts, [loadContacts]);

  const filtered = useMemo(() => {
    if (!search) return contacts;
    const q = search.toLowerCase();
    return contacts.filter((c) =>
      c.fn.toLowerCase().includes(q)
      || (c.emails ?? []).some((e) => e.toLowerCase().includes(q))
      || (c.org ?? "").toLowerCase().includes(q),
    );
  }, [contacts, search]);

  const startEdit = (c: Contact | null) => {
    setEditing(c);
    setDraft(c ? {
      uid: c.uid,
      fn: c.fn,
      emails: c.emails ?? [],
      phones: c.phones ?? [],
      org: c.org ?? "",
      note: c.note ?? "",
    } : emptyDraft);
  };

  const onSave = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId || !bookId) return;
    try {
      if (editing && editing.uid) {
        await updateContact(selectedTenantId, accountId, bookId, editing.uid, draft);
      } else {
        await createContact(selectedTenantId, accountId, bookId, draft);
      }
      startEdit(null);
      loadContacts();
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDelete = async (c: Contact) => {
    if (!selectedTenantId || !bookId) return;
    try {
      await deleteContact(selectedTenantId, accountId, bookId, c.uid);
      loadContacts();
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>Contacts</h2>

      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select value={selectedTenantId ?? ""} onChange={(e) => selectTenant(e.target.value)}>
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </label>
        <label>
          Account
          <input type="text" placeholder="user@tenant" value={accountId} onChange={(e) => setAccountId(e.target.value)} />
        </label>
        <label>
          Address book
          <select value={bookId} onChange={(e) => setBookId(e.target.value)}>
            <option value="">— select —</option>
            {books.map((b) => (
              <option key={b.id} value={b.id}>{b.name}{b.isDefault ? " (default)" : ""}</option>
            ))}
          </select>
        </label>
      </div>

      {error && <p className="kmail-error">{error}</p>}

      <div style={{ display: "flex", gap: "1rem" }}>
        <div style={{ flex: 1 }}>
          <input
            type="search"
            placeholder="Search contacts..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            style={{ width: "100%", marginBottom: "0.5rem" }}
          />
          <button onClick={() => startEdit(null)}>+ New contact</button>
          <ul className="kmail-list">
            {filtered.map((c) => (
              <li key={c.uid}>
                <button className="kmail-link" onClick={() => startEdit(c)}>
                  <strong>{c.fn}</strong>
                  {c.emails && c.emails.length > 0 && <small> · {c.emails[0]}</small>}
                  {c.org && <small> · {c.org}</small>}
                </button>
              </li>
            ))}
            {filtered.length === 0 && <li>No contacts.</li>}
          </ul>
        </div>

        <div style={{ flex: 1 }}>
          <form onSubmit={onSave} style={{ display: "grid", gap: "0.5rem" }}>
            <h3>{editing ? "Edit contact" : "New contact"}</h3>
            <label>
              Full name
              <input type="text" required value={draft.fn} onChange={(e) => setDraft({ ...draft, fn: e.target.value })} />
            </label>
            <label>
              Emails (comma-separated)
              <input type="text" value={(draft.emails ?? []).join(", ")} onChange={(e) => setDraft({ ...draft, emails: splitList(e.target.value) })} />
            </label>
            <label>
              Phones (comma-separated)
              <input type="text" value={(draft.phones ?? []).join(", ")} onChange={(e) => setDraft({ ...draft, phones: splitList(e.target.value) })} />
            </label>
            <label>
              Organisation
              <input type="text" value={draft.org ?? ""} onChange={(e) => setDraft({ ...draft, org: e.target.value })} />
            </label>
            <label>
              Note
              <textarea value={draft.note ?? ""} onChange={(e) => setDraft({ ...draft, note: e.target.value })} />
            </label>
            <div style={{ display: "flex", gap: "0.5rem" }}>
              <button type="submit">{editing ? "Save" : "Create"}</button>
              {editing && (
                <button type="button" onClick={() => onDelete(editing)}>Delete</button>
              )}
              {(editing || draft.fn) && (
                <button type="button" onClick={() => startEdit(null)}>Cancel</button>
              )}
            </div>
          </form>
        </div>
      </div>
    </section>
  );
}

function splitList(s: string): string[] {
  return s.split(",").map((x) => x.trim()).filter(Boolean);
}
