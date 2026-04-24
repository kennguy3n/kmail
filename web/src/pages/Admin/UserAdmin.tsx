import { useCallback, useEffect, useRef, useState } from "react";

import {
  AdminApiError,
  deleteUser,
  listUsers,
  updateUser,
  type TenantUser,
  type UpdateUserInput,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * UserAdmin is the user management console.
 *
 * Lists users for the currently-selected tenant, surfaces the
 * fields that matter to an admin (email, display name, role,
 * status, quota), and exposes per-row **Edit** (inline form) and
 * **Delete** (with a confirm step) actions that drive the REST
 * endpoints exported by `internal/tenant/handlers.go`:
 *
 *   PATCH  /api/v1/tenants/:id/users/:userId
 *   DELETE /api/v1/tenants/:id/users/:userId
 *
 * Shared-inbox membership, alias management, and MLS-epoch
 * plumbing for shared-group membership changes are out of scope
 * for this iteration — they land alongside the KChat-group side
 * of that workflow (docs/SCHEMA.md §5.6).
 */
export default function UserAdmin() {
  const {
    tenants,
    selectedTenantId,
    selectedTenant,
    selectTenant,
    isLoading: tenantsLoading,
    error: tenantsError,
  } = useTenantSelection();

  const [users, setUsers] = useState<TenantUser[] | null>(null);
  const [usersLoading, setUsersLoading] = useState(false);
  const [usersError, setUsersError] = useState<string | null>(null);

  // `editingUserId` toggles the inline edit row. `editDraft` holds
  // the in-flight form values; it is seeded from the user record
  // when editing starts and flushed through `updateUser` on save.
  // `editingUserIdRef` mirrors `editingUserId` so promise handlers
  // can observe the live value instead of a stale closure — matters
  // when an admin clicks Save on one row and then Edit on another
  // before the PATCH resolves.
  const [editingUserId, setEditingUserId] = useState<string | null>(null);
  const editingUserIdRef = useRef<string | null>(null);
  useEffect(() => {
    editingUserIdRef.current = editingUserId;
  }, [editingUserId]);
  const [editDraft, setEditDraft] = useState<UpdateUserInput>({});
  const [saving, setSaving] = useState(false);
  const [rowError, setRowError] = useState<Record<string, string>>({});
  // Pending-delete state gates the destructive action behind a
  // per-row confirm. The user clicks Delete once to arm, then
  // Confirm delete to actually fire the DELETE.
  const [pendingDelete, setPendingDelete] = useState<string | null>(null);

  const loadUsers = useCallback((tenantId: string) => {
    let cancelled = false;
    setUsersLoading(true);
    setUsersError(null);
    listUsers(tenantId)
      .then((list) => {
        if (cancelled) return;
        setUsers(list);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setUsersError(errorMessage(e));
      })
      .finally(() => {
        if (cancelled) return;
        setUsersLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!selectedTenantId) return;
    return loadUsers(selectedTenantId);
  }, [selectedTenantId, loadUsers]);

  const startEdit = (u: TenantUser): void => {
    setEditingUserId(u.id);
    setEditDraft({
      display_name: u.display_name,
      role: u.role,
      status: u.status,
      quota_bytes: u.quota_bytes,
    });
    setRowError((prev) => {
      const { [u.id]: _dropped, ...rest } = prev;
      return rest;
    });
  };

  const cancelEdit = (): void => {
    setEditingUserId(null);
    setEditDraft({});
  };

  const saveEdit = (u: TenantUser): void => {
    if (!selectedTenantId) return;
    setSaving(true);
    setRowError((prev) => {
      const { [u.id]: _dropped, ...rest } = prev;
      return rest;
    });
    // Only send fields the draft actually changed so the server
    // does not stomp on concurrent edits.
    const patch: UpdateUserInput = {};
    if (editDraft.display_name !== undefined && editDraft.display_name !== u.display_name) {
      patch.display_name = editDraft.display_name;
    }
    if (editDraft.role !== undefined && editDraft.role !== u.role) {
      patch.role = editDraft.role;
    }
    if (editDraft.status !== undefined && editDraft.status !== u.status) {
      patch.status = editDraft.status;
    }
    if (editDraft.quota_bytes !== undefined && editDraft.quota_bytes !== u.quota_bytes) {
      patch.quota_bytes = editDraft.quota_bytes;
    }
    if (Object.keys(patch).length === 0) {
      cancelEdit();
      setSaving(false);
      return;
    }
    updateUser(selectedTenantId, u.id, patch)
      .then((updated) => {
        setUsers((current) =>
          current
            ? current.map((row) => (row.id === updated.id ? updated : row))
            : current,
        );
        // Scope the cancel to the row whose save just completed so
        // an admin who started editing a different row while this
        // PATCH was in flight does not lose their in-progress edit.
        if (editingUserIdRef.current === updated.id) {
          setEditingUserId(null);
          setEditDraft({});
        }
      })
      .catch((e: unknown) => {
        setRowError((prev) => ({ ...prev, [u.id]: errorMessage(e) }));
      })
      .finally(() => {
        setSaving(false);
      });
  };

  const onDelete = (u: TenantUser): void => {
    if (!selectedTenantId) return;
    if (pendingDelete !== u.id) {
      setPendingDelete(u.id);
      return;
    }
    setRowError((prev) => {
      const { [u.id]: _dropped, ...rest } = prev;
      return rest;
    });
    deleteUser(selectedTenantId, u.id)
      .then(() => {
        setPendingDelete(null);
        setUsers((current) =>
          current ? current.filter((row) => row.id !== u.id) : current,
        );
      })
      .catch((e: unknown) => {
        setRowError((prev) => ({ ...prev, [u.id]: errorMessage(e) }));
      });
  };

  return (
    <section className="kmail-admin">
      <h2>User admin</h2>

      <div className="kmail-admin-tenant-picker">
        <label htmlFor="kmail-admin-tenant">Tenant:</label>{" "}
        <select
          id="kmail-admin-tenant"
          disabled={tenantsLoading || !tenants || tenants.length === 0}
          value={selectedTenantId ?? ""}
          onChange={(e) => selectTenant(e.target.value)}
        >
          {tenantsLoading && <option value="">Loading…</option>}
          {!tenantsLoading && tenants && tenants.length === 0 && (
            <option value="">No tenants</option>
          )}
          {tenants &&
            tenants.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name} ({t.slug})
              </option>
            ))}
        </select>
        {tenantsError && (
          <span className="kmail-admin-error">
            {" "}Failed to load tenants: {tenantsError}
          </span>
        )}
      </div>

      {!selectedTenant ? (
        <p className="kmail-admin-hint">
          {tenantsLoading
            ? "Loading tenants…"
            : "Select a tenant to manage its users."}
        </p>
      ) : (
        <>
          <p className="kmail-admin-hint">
            Users for <strong>{selectedTenant.name}</strong> ({selectedTenant.slug}).
          </p>

          {usersLoading && <p>Loading users…</p>}
          {usersError && (
            <p className="kmail-admin-error">Failed to load users: {usersError}</p>
          )}

          {users && users.length === 0 && !usersLoading && (
            <p>No users in this tenant.</p>
          )}

          {users && users.length > 0 && (
            <table className="kmail-admin-table">
              <thead>
                <tr>
                  <th>Email</th>
                  <th>Display name</th>
                  <th>Role</th>
                  <th>Status</th>
                  <th>Quota (bytes)</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) =>
                  editingUserId === u.id ? (
                    <tr key={u.id}>
                      <td>{u.email}</td>
                      <td>
                        <input
                          type="text"
                          value={editDraft.display_name ?? ""}
                          onChange={(e) =>
                            setEditDraft((prev) => ({
                              ...prev,
                              display_name: e.target.value,
                            }))
                          }
                        />
                      </td>
                      <td>
                        <select
                          value={editDraft.role ?? u.role}
                          onChange={(e) =>
                            setEditDraft((prev) => ({
                              ...prev,
                              role: e.target.value,
                            }))
                          }
                        >
                          <option value="member">member</option>
                          <option value="admin">admin</option>
                          <option value="owner">owner</option>
                        </select>
                      </td>
                      <td>
                        <select
                          value={editDraft.status ?? u.status}
                          onChange={(e) =>
                            setEditDraft((prev) => ({
                              ...prev,
                              status: e.target.value,
                            }))
                          }
                        >
                          <option value="active">active</option>
                          <option value="suspended">suspended</option>
                          <option value="deleted">deleted</option>
                        </select>
                      </td>
                      <td>
                        <input
                          type="number"
                          min={0}
                          value={
                            editDraft.quota_bytes !== undefined
                              ? editDraft.quota_bytes
                              : u.quota_bytes
                          }
                          onChange={(e) =>
                            setEditDraft((prev) => ({
                              ...prev,
                              quota_bytes: Number(e.target.value),
                            }))
                          }
                        />
                      </td>
                      <td>
                        <button
                          type="button"
                          onClick={() => saveEdit(u)}
                          disabled={saving}
                        >
                          {saving ? "Saving…" : "Save"}
                        </button>{" "}
                        <button type="button" onClick={cancelEdit} disabled={saving}>
                          Cancel
                        </button>
                        {rowError[u.id] && (
                          <div className="kmail-admin-error">{rowError[u.id]}</div>
                        )}
                      </td>
                    </tr>
                  ) : (
                    <tr key={u.id}>
                      <td>{u.email}</td>
                      <td>{u.display_name}</td>
                      <td>{u.role}</td>
                      <td>{u.status}</td>
                      <td>{u.quota_bytes.toLocaleString()}</td>
                      <td>
                        <button type="button" onClick={() => startEdit(u)}>
                          Edit
                        </button>{" "}
                        {pendingDelete === u.id ? (
                          <>
                            <button
                              type="button"
                              onClick={() => onDelete(u)}
                              className="kmail-admin-danger"
                            >
                              Confirm delete
                            </button>{" "}
                            <button
                              type="button"
                              onClick={() => setPendingDelete(null)}
                            >
                              Cancel
                            </button>
                          </>
                        ) : (
                          <button
                            type="button"
                            onClick={() => onDelete(u)}
                            className="kmail-admin-danger"
                          >
                            Delete
                          </button>
                        )}
                        {rowError[u.id] && (
                          <div className="kmail-admin-error">{rowError[u.id]}</div>
                        )}
                      </td>
                    </tr>
                  ),
                )}
              </tbody>
            </table>
          )}
        </>
      )}
    </section>
  );
}

function errorMessage(e: unknown): string {
  if (e instanceof AdminApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}
