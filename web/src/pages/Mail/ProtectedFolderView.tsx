/**
 * ProtectedFolderView lists per-tenant protected folders, their
 * grants, and the access log. Sharing happens within a tenant
 * only — cross-tenant sharing is intentionally out of scope per
 * the do-not-do list.
 */

import { useCallback, useEffect, useState } from "react";

import {
  type FolderAccess,
  type FolderAccessLogEntry,
  type ProtectedFolder,
  createProtectedFolder,
  getProtectedFolderAccessLog,
  listProtectedFolderAccess,
  listProtectedFolders,
  shareProtectedFolder,
  unshareProtectedFolder,
} from "../../api/admin";
import { useTenantSelection } from "../Admin/useTenantSelection";

export default function ProtectedFolderView() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [folders, setFolders] = useState<ProtectedFolder[]>([]);
  const [selected, setSelected] = useState<ProtectedFolder | null>(null);
  const [access, setAccess] = useState<FolderAccess[]>([]);
  const [logEntries, setLogEntries] = useState<FolderAccessLogEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [showShare, setShowShare] = useState(false);

  const [createName, setCreateName] = useState("");
  const [ownerId, setOwnerId] = useState("user-1");
  const [granteeId, setGranteeId] = useState("");
  const [permission, setPermission] =
    useState<FolderAccess["permission"]>("read");

  const reloadFolders = useCallback((tid: string) => {
    listProtectedFolders(tid)
      .then((rows) => setFolders(rows ?? []))
      .catch((e: unknown) => setError(String(e)));
  }, []);

  const reloadDetail = useCallback(
    (tid: string, fid: string) => {
      listProtectedFolderAccess(tid, fid)
        .then((rows) => setAccess(rows ?? []))
        .catch((e: unknown) => setError(String(e)));
      getProtectedFolderAccessLog(tid, fid)
        .then((rows) => setLogEntries(rows ?? []))
        .catch((e: unknown) => setError(String(e)));
    },
    [],
  );

  useEffect(() => {
    if (selectedTenantId) reloadFolders(selectedTenantId);
  }, [selectedTenantId, reloadFolders]);

  useEffect(() => {
    if (selectedTenantId && selected)
      reloadDetail(selectedTenantId, selected.id);
  }, [selectedTenantId, selected, reloadDetail]);

  const onCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId || !createName.trim()) return;
    try {
      await createProtectedFolder(selectedTenantId, ownerId, createName.trim());
      setCreateName("");
      reloadFolders(selectedTenantId);
    } catch (err) {
      setError(String(err));
    }
  };

  const onShare = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId || !selected || !granteeId.trim()) return;
    try {
      await shareProtectedFolder(
        selectedTenantId,
        selected.id,
        selected.owner_id,
        granteeId.trim(),
        permission,
      );
      setGranteeId("");
      setShowShare(false);
      reloadDetail(selectedTenantId, selected.id);
    } catch (err) {
      setError(String(err));
    }
  };

  const onUnshare = async (a: FolderAccess) => {
    if (!selectedTenantId || !selected) return;
    try {
      await unshareProtectedFolder(
        selectedTenantId,
        selected.id,
        selected.owner_id,
        a.grantee_id,
      );
      reloadDetail(selectedTenantId, selected.id);
    } catch (err) {
      setError(String(err));
    }
  };

  return (
    <section className="kmail-admin-page kmail-protected">
      <h2>Protected folders</h2>
      <p>
        Folders shared with named teammates inside the same tenant. Server can
        index and search the contents (unlike Zero-Access Vault), but every
        share / read is recorded in the audit log.
      </p>

      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select
            value={selectedTenantId ?? ""}
            onChange={(e) => selectTenant(e.target.value)}
          >
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </label>
      </div>

      {selectedTenantId && (
        <>
          <form onSubmit={onCreate} className="kmail-protected-create">
            <h3>Create folder</h3>
            <label>
              Owner ID
              <input
                value={ownerId}
                onChange={(e) => setOwnerId(e.target.value)}
                required
              />
            </label>
            <label>
              Folder name
              <input
                value={createName}
                onChange={(e) => setCreateName(e.target.value)}
                required
              />
            </label>
            <button type="submit" disabled={!createName.trim()}>
              Create
            </button>
          </form>

          <h3>Folders</h3>
          {folders.length === 0 ? (
            <p>No protected folders yet.</p>
          ) : (
            <ul className="kmail-protected-list">
              {folders.map((f) => (
                <li key={f.id}>
                  <button
                    type="button"
                    className="kmail-protected-row"
                    onClick={() => setSelected(f)}
                  >
                    <span aria-hidden="true">🔐</span>{" "}
                    <strong>{f.folder_name}</strong>{" "}
                    <span className="kmail-protected-owner">
                      owner: <code>{f.owner_id}</code>
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}

          {selected && (
            <article className="kmail-protected-detail">
              <h3>
                <span aria-hidden="true">🔐</span> {selected.folder_name}
              </h3>
              <button type="button" onClick={() => setShowShare(true)}>
                Share with team member
              </button>

              <h4>Current grants</h4>
              {access.length === 0 ? (
                <p>No grants. Only the owner can read.</p>
              ) : (
                <table>
                  <thead>
                    <tr>
                      <th>Grantee</th>
                      <th>Permission</th>
                      <th>Granted</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {access.map((a) => (
                      <tr key={a.id}>
                        <td><code>{a.grantee_id}</code></td>
                        <td>{a.permission}</td>
                        <td>{a.granted_at}</td>
                        <td>
                          <button type="button" onClick={() => onUnshare(a)}>
                            Revoke
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}

              <h4>Access log</h4>
              {logEntries.length === 0 ? (
                <p>No log entries yet.</p>
              ) : (
                <table className="kmail-protected-log">
                  <thead>
                    <tr><th>When</th><th>Actor</th><th>Action</th></tr>
                  </thead>
                  <tbody>
                    {logEntries.map((l) => (
                      <tr key={l.id}>
                        <td>{l.created_at}</td>
                        <td><code>{l.actor_id}</code></td>
                        <td>{l.action}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </article>
          )}
        </>
      )}

      {showShare && selected && (
        <div className="kmail-modal" role="dialog" aria-modal="true">
          <form className="kmail-modal-content" onSubmit={onShare}>
            <h3>Share &ldquo;{selected.folder_name}&rdquo;</h3>
            <label>
              Grantee user ID
              <input
                value={granteeId}
                onChange={(e) => setGranteeId(e.target.value)}
                required
              />
            </label>
            <label>
              Permission
              <select
                value={permission}
                onChange={(e) =>
                  setPermission(e.target.value as FolderAccess["permission"])
                }
              >
                <option value="read">read</option>
                <option value="read_write">read_write</option>
              </select>
            </label>
            <div className="kmail-modal-actions">
              <button type="button" onClick={() => setShowShare(false)}>
                Cancel
              </button>
              <button type="submit" disabled={!granteeId.trim()}>
                Share
              </button>
            </div>
          </form>
        </div>
      )}

      {error && <p className="kmail-error">{error}</p>}
    </section>
  );
}
