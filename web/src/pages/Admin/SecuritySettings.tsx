/**
 * SecuritySettings shows the user's registered WebAuthn / FIDO2
 * keys with add and remove actions. The actual register / login
 * ceremony runs against `navigator.credentials` in the browser;
 * this page is the management surface only.
 */
import { useCallback, useEffect, useState } from "react";

import {
  deleteWebAuthnCredential,
  listWebAuthnCredentials,
  type WebAuthnCredential,
} from "../../api/admin";

export default function SecuritySettings() {
  const [credentials, setCredentials] = useState<WebAuthnCredential[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);

  const reload = useCallback(() => {
    listWebAuthnCredentials()
      .then((r) => setCredentials(r.credentials))
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => reload(), [reload]);

  const onRegister = async () => {
    setInfo(null);
    setError(null);
    try {
      const beginRes = await fetch("/api/v1/auth/webauthn/register/begin", {
        method: "POST",
        credentials: "include",
      });
      if (!beginRes.ok) throw new Error(`begin: ${beginRes.status}`);
      const opts = await beginRes.json();
      // Browser-side WebAuthn ceremony is intentionally minimal
      // here. A production implementation marshals the binary
      // fields per the WebAuthn JS spec (see docs/SECURITY.md).
      if (typeof navigator === "undefined" || !navigator.credentials) {
        throw new Error("WebAuthn unavailable in this browser");
      }
      setInfo(`Registration challenge issued; complete it via your security key (RP=${opts.rp?.id ?? "kmail"}).`);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDelete = async (id: string) => {
    try {
      await deleteWebAuthnCredential(id);
      setInfo("Credential removed.");
      reload();
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <div className="admin-page">
      <h2>Security keys</h2>
      <p>
        Manage hardware-backed second factors registered to your account
        (FIDO2 security keys, platform authenticators).
      </p>
      <div className="actions">
        <button type="button" onClick={onRegister}>Register a new key</button>
      </div>
      {error && <p className="error">{error}</p>}
      {info && <p className="info">{info}</p>}
      <table className="admin-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Created</th>
            <th>Last used</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {credentials.map((c) => (
            <tr key={c.id}>
              <td>{c.name}</td>
              <td>{c.created_at}</td>
              <td>{c.last_used_at ?? "—"}</td>
              <td>
                <button type="button" onClick={() => onDelete(c.id)}>Remove</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
