import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import {
  createSharedInbox,
  createUser,
  deleteUser,
  listSharedInboxes,
  listUsers,
  updateUser,
} from "../../api/admin";
import type { SharedInbox, User, UserPatch } from "../../types";

const DEV_TENANT_ID = "00000000-0000-0000-0000-000000000000";

function resolveTenantId(): string {
  const params = new URLSearchParams(window.location.search);
  return params.get("tenantId") ?? DEV_TENANT_ID;
}

function formatBytes(n: number): string {
  if (!n) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${v.toFixed(1)} ${units[u]}`;
}

export default function UserAdmin() {
  const [tenantId] = useState(resolveTenantId);
  const [users, setUsers] = useState<User[]>([]);
  const [inboxes, setInboxes] = useState<SharedInbox[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const [newUser, setNewUser] = useState({
    email: "",
    displayName: "",
    role: "member",
  });
  const [newInbox, setNewInbox] = useState({ address: "", displayName: "" });

  async function reload() {
    try {
      setError(null);
      const [u, s] = await Promise.all([
        listUsers(tenantId),
        listSharedInboxes(tenantId),
      ]);
      setUsers(u);
      setInboxes(s);
    } catch (err) {
      setError((err as Error).message);
    }
  }
  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tenantId]);

  async function onCreateUser(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await createUser(tenantId, newUser);
      setNewUser({ email: "", displayName: "", role: "member" });
      await reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onPatch(userId: string, patch: UserPatch) {
    setBusy(true);
    try {
      await updateUser(tenantId, userId, patch);
      await reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onDelete(userId: string) {
    if (!confirm("Delete this user?")) return;
    setBusy(true);
    try {
      await deleteUser(tenantId, userId);
      await reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onCreateInbox(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      await createSharedInbox(tenantId, newInbox);
      setNewInbox({ address: "", displayName: "" });
      await reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <section>
      <h2>User admin</h2>
      <p style={{ fontSize: "0.9em", color: "#666" }}>Tenant ID: {tenantId}</p>
      {error && <div style={{ color: "crimson" }}>Error: {error}</div>}

      <h3>Add user</h3>
      <form onSubmit={onCreateUser} style={{ marginBottom: 16 }}>
        <input
          placeholder="email"
          type="email"
          value={newUser.email}
          onChange={(e) => setNewUser({ ...newUser, email: e.target.value })}
          required
        />{" "}
        <input
          placeholder="display name"
          value={newUser.displayName}
          onChange={(e) =>
            setNewUser({ ...newUser, displayName: e.target.value })
          }
          required
        />{" "}
        <select
          value={newUser.role}
          onChange={(e) => setNewUser({ ...newUser, role: e.target.value })}
        >
          <option value="member">Member</option>
          <option value="admin">Admin</option>
          <option value="owner">Owner</option>
        </select>{" "}
        <button type="submit" disabled={busy}>
          Create
        </button>
      </form>

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #ddd" }}>
            <th>Email</th>
            <th>Name</th>
            <th>Role</th>
            <th>Status</th>
            <th>Quota</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr
              key={u.id}
              style={{ borderBottom: "1px solid #eee" }}
            >
              <td>{u.email}</td>
              <td>{u.displayName}</td>
              <td>
                <select
                  value={u.role}
                  onChange={(e) =>
                    onPatch(u.id, { role: e.target.value as User["role"] })
                  }
                  disabled={busy}
                >
                  <option value="member">Member</option>
                  <option value="admin">Admin</option>
                  <option value="owner">Owner</option>
                </select>
              </td>
              <td>
                <select
                  value={u.status}
                  onChange={(e) =>
                    onPatch(u.id, {
                      status: e.target.value as User["status"],
                    })
                  }
                  disabled={busy}
                >
                  <option value="active">Active</option>
                  <option value="suspended">Suspended</option>
                </select>
              </td>
              <td>{formatBytes(u.mailboxQuotaBytes)}</td>
              <td>
                <button onClick={() => onDelete(u.id)} disabled={busy}>
                  Delete
                </button>
              </td>
            </tr>
          ))}
          {users.length === 0 && (
            <tr>
              <td colSpan={6} style={{ color: "#777", padding: 12 }}>
                No users yet.
              </td>
            </tr>
          )}
        </tbody>
      </table>

      <h3 style={{ marginTop: 24 }}>Shared inboxes</h3>
      <form onSubmit={onCreateInbox} style={{ marginBottom: 16 }}>
        <input
          placeholder="address (e.g. alerts@example.com)"
          type="email"
          value={newInbox.address}
          onChange={(e) =>
            setNewInbox({ ...newInbox, address: e.target.value })
          }
          required
        />{" "}
        <input
          placeholder="display name"
          value={newInbox.displayName}
          onChange={(e) =>
            setNewInbox({ ...newInbox, displayName: e.target.value })
          }
          required
        />{" "}
        <button type="submit" disabled={busy}>
          Add shared inbox
        </button>
      </form>
      <ul>
        {inboxes.map((i) => (
          <li key={i.id}>
            <strong>{i.displayName}</strong> — {i.address}
          </li>
        ))}
        {inboxes.length === 0 && (
          <li style={{ color: "#777" }}>No shared inboxes yet.</li>
        )}
      </ul>
    </section>
  );
}
