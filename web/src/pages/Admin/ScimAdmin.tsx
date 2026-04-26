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
  const [info, setInfo] = useState<string | null>(null);
  const [newToken, setNewToken] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [pendingRevoke, setPendingRevoke] = useState<ScimToken | null>(null);
  const [generating, setGenerating] = useState(false);

  const reload = useCallback((tid: string) => {
    setLoading(true);
    listScimTokens(tid)
      .then(setTokens)
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onGenerate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId) return;
    setGenerating(true);
    try {
      const token = await generateScimToken(selectedTenantId, description);
      setNewToken(token.token ?? null);
      setDescription("");
      setInfo("Token generated. Copy it now — it cannot be shown again.");
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setGenerating(false);
    }
  };

  const confirmRevoke = (t: ScimToken) => setPendingRevoke(t);
  const onRevokeConfirmed = async () => {
    if (!selectedTenantId || !pendingRevoke) return;
    try {
      await revokeScimToken(selectedTenantId, pendingRevoke.id);
      setInfo("Token revoked.");
      setPendingRevoke(null);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const copyToken = async () => {
    if (!newToken) return;
    try {
      await navigator.clipboard.writeText(newToken);
      setInfo("Token copied to clipboard.");
    } catch {
      setError("Clipboard write failed; copy the token manually.");
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
            <button type="submit" disabled={generating}>
              {generating ? "Generating…" : "Generate"}
            </button>
          </form>

          {newToken && (
            <div className="kmail-admin-card" role="region" aria-label="New SCIM token">
              <h4>New token (copy now — it will not be shown again)</h4>
              <code style={{ wordBreak: "break-all", display: "block" }}>{newToken}</code>
              <div style={{ display: "flex", gap: "0.5rem", marginTop: "0.5rem" }}>
                <button onClick={copyToken}>Copy token</button>
                <button onClick={() => setNewToken(null)}>Hide</button>
              </div>
            </div>
          )}

          <h3>Active tokens</h3>
          {loading && <p>Loading…</p>}
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
                      <button onClick={() => confirmRevoke(t)}>Revoke</button>
                    )}
                  </td>
                </tr>
              ))}
              {!loading && tokens.length === 0 && (
                <tr><td colSpan={4}>No tokens yet — generate one above.</td></tr>
              )}
            </tbody>
          </table>
        </>
      )}

      {pendingRevoke && (
        <div role="dialog" aria-modal="true" className="kmail-modal">
          <div className="kmail-modal-body">
            <p>
              Revoke token <strong>{pendingRevoke.description || pendingRevoke.id}</strong>?
              Provisioning calls using this token will fail immediately.
            </p>
            <div style={{ display: "flex", gap: "0.5rem" }}>
              <button onClick={onRevokeConfirmed}>Revoke</button>
              <button onClick={() => setPendingRevoke(null)}>Cancel</button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
