/**
 * Contacts view — lists CardDAV address books, contacts, and the
 * tenant-wide Global Address List. Supports full CRUD plus vCard
 * import / export and contact groups.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  createContact,
  deleteContact,
  exportVCard,
  getGlobalAddressList,
  importVCard,
  listAddressBooks,
  listContacts,
  searchGlobalAddressList,
  updateContact,
} from "../../api/contacts";
import { useTenantSelection } from "../Admin/useTenantSelection";
import type { AddressBook, Contact, ContactDraft, GalEntry } from "../../types";

const emptyDraft: ContactDraft = {
  fn: "",
  emails: [],
  phones: [],
  org: "",
  note: "",
  photoUrl: "",
  groups: [],
};

type Tab = "personal" | "global";

export default function ContactsView() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [tab, setTab] = useState<Tab>("personal");
  const [accountId, setAccountId] = useState("");
  const [books, setBooks] = useState<AddressBook[]>([]);
  const [bookId, setBookId] = useState<string>("");
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [search, setSearch] = useState("");
  const [groupFilter, setGroupFilter] = useState<string>("");
  const [editing, setEditing] = useState<Contact | null>(null);
  const [draft, setDraft] = useState<ContactDraft>(emptyDraft);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Contact | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  // GAL state
  const [galEntries, setGalEntries] = useState<GalEntry[]>([]);
  const [galQuery, setGalQuery] = useState("");

  const loadBooks = useCallback(() => {
    if (!selectedTenantId || !accountId) return;
    setLoading(true);
    listAddressBooks(selectedTenantId, accountId)
      .then((b) => {
        setBooks(b);
        setBookId(b.find((x) => x.isDefault)?.id ?? b[0]?.id ?? "");
      })
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, [selectedTenantId, accountId]);

  const loadContacts = useCallback(() => {
    if (!selectedTenantId || !accountId || !bookId) return;
    setLoading(true);
    listContacts(selectedTenantId, accountId, bookId)
      .then(setContacts)
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, [selectedTenantId, accountId, bookId]);

  const loadGAL = useCallback(() => {
    if (!selectedTenantId) return;
    setLoading(true);
    const fetch = galQuery.trim()
      ? searchGlobalAddressList(selectedTenantId, galQuery.trim())
      : getGlobalAddressList(selectedTenantId);
    fetch
      .then(setGalEntries)
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, [selectedTenantId, galQuery]);

  useEffect(loadBooks, [loadBooks]);
  useEffect(loadContacts, [loadContacts]);
  useEffect(() => {
    if (tab === "global") loadGAL();
  }, [tab, loadGAL]);

  const allGroups = useMemo(() => {
    const s = new Set<string>();
    for (const c of contacts) for (const g of c.groups ?? []) s.add(g);
    return Array.from(s).sort();
  }, [contacts]);

  const filtered = useMemo(() => {
    const q = search.toLowerCase();
    return contacts.filter((c) => {
      if (groupFilter && !(c.groups ?? []).includes(groupFilter)) return false;
      if (!q) return true;
      return (
        c.fn.toLowerCase().includes(q) ||
        (c.emails ?? []).some((e) => e.toLowerCase().includes(q)) ||
        (c.org ?? "").toLowerCase().includes(q)
      );
    });
  }, [contacts, search, groupFilter]);

  const startEdit = (c: Contact | null) => {
    setEditing(c);
    setDraft(
      c
        ? {
            uid: c.uid,
            fn: c.fn,
            emails: c.emails ?? [],
            phones: c.phones ?? [],
            org: c.org ?? "",
            note: c.note ?? "",
            photoUrl: c.photoUrl ?? "",
            groups: c.groups ?? [],
          }
        : emptyDraft,
    );
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
      setInfo(editing ? "Contact updated." : "Contact created.");
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const confirmDelete = (c: Contact) => setPendingDelete(c);
  const onDeleteConfirmed = async () => {
    if (!pendingDelete || !selectedTenantId || !bookId) return;
    try {
      await deleteContact(selectedTenantId, accountId, bookId, pendingDelete.uid);
      setPendingDelete(null);
      startEdit(null);
      loadContacts();
      setInfo("Contact deleted.");
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const onImportFile = async (file: File) => {
    if (!selectedTenantId || !bookId) return;
    const text = await file.text();
    try {
      const out = await importVCard(selectedTenantId, accountId, bookId, text);
      setInfo(`Imported ${out.created} contact(s); ${out.failed} failed.`);
      loadContacts();
    } catch (err: unknown) {
      setError(String(err));
    }
  };

  const onExport = async () => {
    if (!selectedTenantId || !bookId) return;
    try {
      const text = await exportVCard(selectedTenantId, accountId, bookId);
      const blob = new Blob([text], { type: "text/vcard;charset=utf-8" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = "contacts.vcf";
      a.click();
      URL.revokeObjectURL(url);
    } catch (err: unknown) {
      setError(String(err));
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
        {tab === "personal" && (
          <>
            <label>
              Account
              <input
                type="text"
                placeholder="user@tenant"
                value={accountId}
                onChange={(e) => setAccountId(e.target.value)}
              />
            </label>
            <label>
              Address book
              <select value={bookId} onChange={(e) => setBookId(e.target.value)}>
                <option value="">— select —</option>
                {books.map((b) => (
                  <option key={b.id} value={b.id}>
                    {b.name}{b.isDefault ? " (default)" : ""}
                  </option>
                ))}
              </select>
            </label>
          </>
        )}
      </div>

      <div role="tablist" style={{ display: "flex", gap: "0.5rem", marginBottom: "1rem" }}>
        <button
          role="tab"
          aria-selected={tab === "personal"}
          onClick={() => setTab("personal")}
        >
          Personal
        </button>
        <button
          role="tab"
          aria-selected={tab === "global"}
          onClick={() => setTab("global")}
        >
          Global Directory
        </button>
      </div>

      {error && (
        <p className="kmail-error" role="alert">
          {error} <button onClick={() => setError(null)}>dismiss</button>
        </p>
      )}
      {info && (
        <p className="kmail-info" role="status">
          {info} <button onClick={() => setInfo(null)}>dismiss</button>
        </p>
      )}
      {loading && <p>Loading…</p>}

      {tab === "personal" ? (
        <div style={{ display: "flex", gap: "1rem" }}>
          <div style={{ flex: 1 }}>
            <div style={{ display: "flex", gap: "0.5rem", marginBottom: "0.5rem" }}>
              <input
                type="search"
                placeholder="Search contacts..."
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                style={{ flex: 1 }}
              />
              {allGroups.length > 0 && (
                <select value={groupFilter} onChange={(e) => setGroupFilter(e.target.value)}>
                  <option value="">All groups</option>
                  {allGroups.map((g) => (
                    <option key={g} value={g}>{g}</option>
                  ))}
                </select>
              )}
            </div>
            <div style={{ display: "flex", gap: "0.5rem", marginBottom: "0.5rem" }}>
              <button onClick={() => startEdit(null)}>+ New contact</button>
              <button onClick={() => fileInputRef.current?.click()}>Import vCard</button>
              <button onClick={onExport} disabled={!bookId || contacts.length === 0}>
                Export vCard
              </button>
              <input
                ref={fileInputRef}
                type="file"
                accept=".vcf,text/vcard"
                style={{ display: "none" }}
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) onImportFile(f);
                  e.target.value = "";
                }}
              />
            </div>
            <ul className="kmail-list">
              {filtered.map((c) => (
                <li key={c.uid}>
                  <button className="kmail-link" onClick={() => startEdit(c)}>
                    {c.photoUrl && (
                      <img
                        src={c.photoUrl}
                        alt=""
                        style={{ width: 24, height: 24, borderRadius: "50%", marginRight: 8, verticalAlign: "middle" }}
                      />
                    )}
                    <strong>{c.fn}</strong>
                    {c.emails && c.emails.length > 0 && <small> · {c.emails[0]}</small>}
                    {c.org && <small> · {c.org}</small>}
                    {c.groups && c.groups.length > 0 && (
                      <small> · {c.groups.map((g) => `[${g}]`).join(" ")}</small>
                    )}
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
                <input
                  type="text"
                  required
                  value={draft.fn}
                  onChange={(e) => setDraft({ ...draft, fn: e.target.value })}
                />
              </label>
              <label>
                Emails (comma-separated)
                <input
                  type="text"
                  value={(draft.emails ?? []).join(", ")}
                  onChange={(e) => setDraft({ ...draft, emails: splitList(e.target.value) })}
                />
              </label>
              <label>
                Phones (comma-separated)
                <input
                  type="text"
                  value={(draft.phones ?? []).join(", ")}
                  onChange={(e) => setDraft({ ...draft, phones: splitList(e.target.value) })}
                />
              </label>
              <label>
                Organisation
                <input
                  type="text"
                  value={draft.org ?? ""}
                  onChange={(e) => setDraft({ ...draft, org: e.target.value })}
                />
              </label>
              <label>
                Photo URL
                <input
                  type="url"
                  value={draft.photoUrl ?? ""}
                  onChange={(e) => setDraft({ ...draft, photoUrl: e.target.value })}
                />
              </label>
              <label>
                Groups / labels (comma-separated)
                <input
                  type="text"
                  value={(draft.groups ?? []).join(", ")}
                  onChange={(e) => setDraft({ ...draft, groups: splitList(e.target.value) })}
                />
              </label>
              <label>
                Note
                <textarea
                  value={draft.note ?? ""}
                  onChange={(e) => setDraft({ ...draft, note: e.target.value })}
                />
              </label>
              <div style={{ display: "flex", gap: "0.5rem" }}>
                <button type="submit">{editing ? "Save" : "Create"}</button>
                {editing && (
                  <button type="button" onClick={() => confirmDelete(editing)}>
                    Delete
                  </button>
                )}
                {(editing || draft.fn) && (
                  <button type="button" onClick={() => startEdit(null)}>
                    Cancel
                  </button>
                )}
              </div>
            </form>
          </div>
        </div>
      ) : (
        <div>
          <div style={{ display: "flex", gap: "0.5rem", marginBottom: "0.5rem" }}>
            <input
              type="search"
              placeholder="Search the directory..."
              value={galQuery}
              onChange={(e) => setGalQuery(e.target.value)}
              style={{ flex: 1 }}
            />
            <button onClick={loadGAL}>Refresh</button>
          </div>
          <ul className="kmail-list">
            {galEntries.map((g) => (
              <li key={`${g.email}`}>
                <strong>{g.display_name || g.email}</strong>
                {g.email && <small> · {g.email}</small>}
                {g.org && <small> · {g.org}</small>}
                {g.phone && <small> · {g.phone}</small>}
              </li>
            ))}
            {galEntries.length === 0 && <li>No directory entries.</li>}
          </ul>
        </div>
      )}

      {pendingDelete && (
        <div role="dialog" aria-modal="true" className="kmail-modal">
          <div className="kmail-modal-body">
            <p>
              Delete <strong>{pendingDelete.fn}</strong>? This cannot be undone.
            </p>
            <div style={{ display: "flex", gap: "0.5rem" }}>
              <button onClick={onDeleteConfirmed}>Delete</button>
              <button onClick={() => setPendingDelete(null)}>Cancel</button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

function splitList(s: string): string[] {
  return s.split(",").map((x) => x.trim()).filter(Boolean);
}
