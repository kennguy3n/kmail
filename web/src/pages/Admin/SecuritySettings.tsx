/**
 * SecuritySettings shows the user's registered WebAuthn / FIDO2
 * keys with add and remove actions, plus a TOTP fallback tab.
 *
 * The actual WebAuthn register / login ceremony runs against
 * `navigator.credentials` in the browser; this page is the
 * management surface only. The TOTP tab carries the full enrol +
 * verify flow because that is purely an HTTP exchange — no native
 * browser API.
 *
 * The page is tenant-scoped because credentials are stored per
 * (tenant, user) pair: in dev mode the OIDC bypass needs the
 * X-KMail-Dev-Tenant-Id header (populated by adminAuthHeaders),
 * and in prod the same header lets a tenant admin select which
 * workspace's credentials to manage.
 */
import { useCallback, useEffect, useState } from "react";

import {
  deleteWebAuthnCredential,
  listWebAuthnCredentials,
  type WebAuthnCredential,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

type Tab = "webauthn" | "totp";

interface TOTPStatus {
  enrolled: boolean;
  enabled: boolean;
}

export default function SecuritySettings() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [tab, setTab] = useState<Tab>("webauthn");
  const [credentials, setCredentials] = useState<WebAuthnCredential[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    listWebAuthnCredentials(tid)
      .then((r) => setCredentials(r.credentials))
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onRegister = async () => {
    setInfo(null);
    setError(null);
    if (!selectedTenantId) {
      setError("Select a tenant first.");
      return;
    }
    try {
      const beginRes = await fetch("/api/v1/auth/webauthn/register/begin", {
        method: "POST",
        credentials: "include",
        headers: { "X-KMail-Dev-Tenant-Id": selectedTenantId },
      });
      if (!beginRes.ok) throw new Error(`begin: ${beginRes.status}`);
      const opts = await beginRes.json();
      if (typeof navigator === "undefined" || !navigator.credentials) {
        throw new Error("WebAuthn unavailable in this browser");
      }
      setInfo(`Registration challenge issued; complete it via your security key (RP=${opts.rp?.id ?? "kmail"}).`);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDelete = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await deleteWebAuthnCredential(selectedTenantId, id);
      setInfo("Credential removed.");
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <div className="admin-page">
      <h2>Security</h2>
      <p>
        Manage hardware-backed second factors (FIDO2 security keys, platform
        authenticators) and the TOTP fallback for your account.
      </p>
      <div className="tenant-picker">
        <label>
          Tenant{" "}
          <select
            value={selectedTenantId ?? ""}
            onChange={(e) => selectTenant(e.target.value)}
          >
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
        </label>
      </div>
      <nav className="tabs" style={{ display: "flex", gap: "12px", margin: "12px 0" }}>
        <button type="button" onClick={() => setTab("webauthn")} aria-pressed={tab === "webauthn"}>
          Security keys (WebAuthn)
        </button>
        <button type="button" onClick={() => setTab("totp")} aria-pressed={tab === "totp"}>
          TOTP (authenticator app)
        </button>
      </nav>
      {error && <p className="error">{error}</p>}
      {info && <p className="info">{info}</p>}
      {tab === "webauthn" && (
        <section>
          <div className="actions">
            <button type="button" onClick={onRegister} disabled={!selectedTenantId}>
              Register a new key
            </button>
          </div>
          {selectedTenantId && (
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
          )}
        </section>
      )}
      {tab === "totp" && <TOTPSection tenantId={selectedTenantId ?? ""} />}
    </div>
  );
}

function TOTPSection({ tenantId }: { tenantId: string }) {
  const [status, setStatus] = useState<TOTPStatus | null>(null);
  const [otpauthURI, setOtpauthURI] = useState<string | null>(null);
  const [secret, setSecret] = useState<string | null>(null);
  const [code, setCode] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const headers = useCallback((): Record<string, string> => {
    return tenantId ? { "X-KMail-Dev-Tenant-Id": tenantId } : {};
  }, [tenantId]);

  const reload = useCallback(async () => {
    if (!tenantId) return;
    try {
      const res = await fetch("/api/v1/auth/totp/status", {
        credentials: "include",
        headers: headers(),
      });
      if (!res.ok) throw new Error(`status: ${res.status}`);
      setStatus(await res.json());
    } catch (e: unknown) {
      setError(String(e));
    }
  }, [tenantId, headers]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const enroll = async () => {
    setError(null);
    setBusy(true);
    try {
      const res = await fetch("/api/v1/auth/totp/enroll", {
        method: "POST",
        credentials: "include",
        headers: headers(),
      });
      if (!res.ok) throw new Error(`enroll: ${res.status}`);
      const body = (await res.json()) as { otpauth_uri: string; secret: string };
      setOtpauthURI(body.otpauth_uri);
      setSecret(body.secret);
      setRecoveryCodes(null);
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const verify = async () => {
    setError(null);
    setBusy(true);
    try {
      const res = await fetch("/api/v1/auth/totp/verify", {
        method: "POST",
        credentials: "include",
        headers: { ...headers(), "Content-Type": "application/json" },
        body: JSON.stringify({ code }),
      });
      if (!res.ok) throw new Error(`verify: ${res.status}`);
      const body = (await res.json()) as { recovery_codes: string[] };
      setRecoveryCodes(body.recovery_codes);
      setOtpauthURI(null);
      setSecret(null);
      setCode("");
      await reload();
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const disable = async () => {
    setError(null);
    setBusy(true);
    try {
      const res = await fetch("/api/v1/auth/totp", {
        method: "DELETE",
        credentials: "include",
        headers: headers(),
      });
      if (!res.ok) throw new Error(`disable: ${res.status}`);
      setRecoveryCodes(null);
      await reload();
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const downloadRecovery = () => {
    if (!recoveryCodes) return;
    const blob = new Blob([recoveryCodes.join("\n") + "\n"], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "kmail-totp-recovery-codes.txt";
    a.click();
    URL.revokeObjectURL(url);
  };

  if (!tenantId) return <p className="muted">Select a tenant to manage TOTP.</p>;
  return (
    <section>
      {error && <p className="error">{error}</p>}
      <p>
        Status:{" "}
        {status?.enabled ? <strong>enabled</strong> : status?.enrolled ? "enrolment in progress" : "not enrolled"}
      </p>
      {!status?.enabled && (
        <div className="actions">
          <button type="button" onClick={enroll} disabled={busy}>
            {status?.enrolled ? "Restart enrolment" : "Begin enrolment"}
          </button>
        </div>
      )}
      {otpauthURI && secret && (
        <div className="totp-enrol">
          <p>
            Scan the QR code below with your authenticator app, or enter the
            secret manually: <code>{secret}</code>
          </p>
          <img
            alt="TOTP QR code"
            src={`https://api.qrserver.com/v1/create-qr-code/?size=180x180&data=${encodeURIComponent(otpauthURI)}`}
          />
          <p className="muted">URI: {otpauthURI}</p>
          <label>
            6-digit code:{" "}
            <input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              inputMode="numeric"
              maxLength={6}
            />
          </label>
          <button type="button" onClick={verify} disabled={busy || code.length !== 6}>
            Verify and enable
          </button>
        </div>
      )}
      {recoveryCodes && (
        <div className="totp-recovery">
          <h3>Recovery codes</h3>
          <p className="muted">
            Save these codes somewhere safe — each works once if you lose access
            to your authenticator app.
          </p>
          <ul>
            {recoveryCodes.map((c) => (
              <li key={c}>
                <code>{c}</code>
              </li>
            ))}
          </ul>
          <button type="button" onClick={downloadRecovery}>Download as .txt</button>
        </div>
      )}
      {status?.enabled && (
        <div className="actions">
          <button type="button" onClick={disable} disabled={busy}>
            Disable TOTP
          </button>
        </div>
      )}
    </section>
  );
}
