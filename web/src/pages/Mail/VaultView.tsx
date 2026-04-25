/**
 * VaultView is the user-facing UI for the Phase 5 Zero-Access
 * Vault. Each folder is in StrictZK mode (server-side search and
 * push previews are intentionally unavailable). Users see the
 * folder list with a lock icon, can create new vault folders, and
 * can drill into a single folder to view its encryption metadata.
 *
 * This page does not opt mailboxes into vault mode by default —
 * each folder is created explicitly. The do-not-do list bans
 * making every mailbox zero-access by default.
 */

import { useCallback, useEffect, useState } from "react";

import {
  createVaultFolder,
  deleteVaultFolder,
  listVaultFolders,
  type VaultFolder,
} from "../../api/admin";
import { useTenantSelection } from "../Admin/useTenantSelection";

export default function VaultView() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [folders, setFolders] = useState<VaultFolder[]>([]);
  const [selected, setSelected] = useState<VaultFolder | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [userId, setUserId] = useState("user-1");
  const [acknowledged, setAcknowledged] = useState(false);

  const reload = useCallback(
    (tid: string) => {
      listVaultFolders(tid)
        .then((rows) => setFolders(rows ?? []))
        .catch((e: unknown) => setError(String(e)));
    },
    [],
  );

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId || !name.trim() || !acknowledged) return;
    try {
      await createVaultFolder(selectedTenantId, userId, name.trim());
      setName("");
      setAcknowledged(false);
      reload(selectedTenantId);
    } catch (err) {
      setError(String(err));
    }
  };

  const onDelete = async (folder: VaultFolder) => {
    if (!selectedTenantId) return;
    if (!confirm(`Delete vault folder "${folder.folder_name}"?`)) return;
    try {
      await deleteVaultFolder(selectedTenantId, folder.id);
      if (selected?.id === folder.id) setSelected(null);
      reload(selectedTenantId);
    } catch (err) {
      setError(String(err));
    }
  };

  return (
    <section className="kmail-admin-page kmail-vault">
      <h2>Zero-Access Vault</h2>
      <p>
        Vault folders are encrypted client-side in StrictZK mode. The KMail
        server cannot read, search, or preview their contents.
      </p>
      <p className="kmail-warning">
        <strong>No server-side search available.</strong> Spam scanning,
        push previews, and admin recovery are also disabled for these
        folders by design.
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
          <form onSubmit={onCreate} className="kmail-vault-create">
            <h3>Create vault folder</h3>
            <label>
              User ID
              <input
                value={userId}
                onChange={(e) => setUserId(e.target.value)}
                required
              />
            </label>
            <label>
              Folder name
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Legal contracts"
                required
              />
            </label>
            <label className="kmail-vault-confirm">
              <input
                type="checkbox"
                checked={acknowledged}
                onChange={(e) => setAcknowledged(e.target.checked)}
              />
              I understand the server cannot search this folder.
            </label>
            <button type="submit" disabled={!acknowledged || !name.trim()}>
              Create vault folder
            </button>
          </form>

          <h3>Folders</h3>
          {folders.length === 0 ? (
            <p>No vault folders yet for this tenant.</p>
          ) : (
            <ul className="kmail-vault-list">
              {folders.map((f) => (
                <li key={f.id}>
                  <button
                    type="button"
                    className="kmail-vault-row"
                    onClick={() => setSelected(f)}
                  >
                    <span aria-hidden="true">🔒</span>{" "}
                    <strong>{f.folder_name}</strong>{" "}
                    <span className="kmail-vault-badge">StrictZK</span>{" "}
                    <span className="kmail-vault-badge kmail-vault-badge-warning">
                      No server-side search
                    </span>
                  </button>{" "}
                  <button type="button" onClick={() => onDelete(f)}>
                    Delete
                  </button>
                </li>
              ))}
            </ul>
          )}

          {selected && <Detail folder={selected} />}
        </>
      )}

      {error && <p className="kmail-error">{error}</p>}
    </section>
  );
}

function Detail({ folder }: { folder: VaultFolder }) {
  return (
    <article className="kmail-vault-detail">
      <h3>
        <span aria-hidden="true">🔒</span> {folder.folder_name}
      </h3>
      <dl>
        <dt>Encryption mode</dt>
        <dd>{folder.encryption_mode}</dd>
        <dt>Key algorithm</dt>
        <dd>{folder.key_algorithm}</dd>
        <dt>Wrapped DEK</dt>
        <dd>{folder.wrapped_dek ? <code>{folder.wrapped_dek.slice(0, 16)}…</code> : <em>not yet provisioned</em>}</dd>
        <dt>Created</dt>
        <dd>{folder.created_at}</dd>
      </dl>
      <p className="kmail-vault-note">
        This folder is encrypted end-to-end. The server stores only opaque
        wrapped key material; plaintext keys never leave the client.
      </p>
    </article>
  );
}
