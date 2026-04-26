/**
 * SCIM 2.0 token management. Lets a tenant admin generate and
 * revoke bearer tokens used by their identity provider to drive
 * SCIM provisioning at `/scim/v2/Users` and `/scim/v2/Groups`.
 */

import { useCallback, useEffect, useState } from "react";

import {
  generateScimToken,
  listScimTokens,
  revokeScimToken,
  type ScimToken,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function ScimAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [tokens, setTokens] = useState<ScimToken[]>([]);
  const [description, setDescription] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    listScimTokens(tid).then(setTokens).catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onGenerate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId) return;
    try {
      const token = await generateScimToken(selectedTenantId, description);
      setNewToken(token.token ?? null);
      setDescription("");
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onRevoke = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await revokeScimToken(selectedTenantId, id);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <section className="kmail-admin-page">
      <h2>SCIM 2.0 provisioning</h2>
      <p className="kmail-admin-help">
        Generate bearer tokens that your identity provider (Okta, Azure AD, etc.) can use
        to provision users and groups via <code>/scim/v2/Users</code> and{" "}
        <code>/scim/v2/Groups</code>. Tokens are tenant-scoped and cannot be retrieved
        again after generation — store them in your IdP's secret manager.
      </p>

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
      </div>

      {error && <p className="kmail-error">{error}</p>}

      {selectedTenantId && (
        <>
          <form onSubmit={onGenerate} style={{ display: "grid", gap: "0.5rem", maxWidth: 480 }}>
            <h3>Generate token</h3>
            <label>
              Description
              <input
                type="text"
                value={description}
                placeholder="e.g. Okta production tenant"
                onChange={(e) => setDescription(e.target.value)}
              />
            </label>
            <button type="submit">Generate</button>
          </form>

          {newToken && (
            <div className="kmail-admin-card">
              <h4>New token (copy now — it will not be shown again)</h4>
              <code style={{ wordBreak: "break-all" }}>{newToken}</code>
            </div>
          )}

          <h3>Active tokens</h3>
          <table className="kmail-admin-table">
            <thead>
              <tr>
                <th>Description</th>
                <th>Created</th>
                <th>Status</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {tokens.map((t) => (
                <tr key={t.id}>
                  <td>{t.description}</td>
                  <td>{new Date(t.created_at).toLocaleString()}</td>
                  <td>{t.revoked_at ? "revoked" : "active"}</td>
                  <td>
                    {!t.revoked_at && (
                      <button onClick={() => onRevoke(t.id)}>Revoke</button>
                    )}
                  </td>
                </tr>
              ))}
              {tokens.length === 0 && (
                <tr><td colSpan={4}>No tokens yet.</td></tr>
              )}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}
