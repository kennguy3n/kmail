/**
 * CmkAdmin manages customer-managed encryption keys for tenants
 * on the privacy plan. Admins paste a PEM-encoded RSA public key
 * (or upload a `.pem` file), see the active key alongside its
 * fingerprint, rotate to a fresh key (deprecating the old one),
 * and revoke a key with confirmation.
 *
 * Backed by `internal/cmk` (migration 025 + RLS-protected
 * `customer_managed_keys` table). The handler enforces the
 * privacy-plan gate; this page additionally surfaces a friendly
 * banner so non-privacy tenants see why the form is disabled.
 */

import { useCallback, useEffect, useState } from "react";

import {
  type CmkKey,
  getActiveCmkKey,
  type HsmConfig,
  type HsmRegistration,
  listCmkKeys,
  listHsmConfigs,
  registerCmkKey,
  registerHsmKey,
  revokeCmkKey,
  rotateCmkKey,
  testHsmConnection,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

type Tab = "pem" | "hsm";

export default function CmkAdmin() {
  const { tenants, selectedTenantId, selectedTenant, selectTenant } =
    useTenantSelection();
  const [tab, setTab] = useState<Tab>("pem");
  const [keys, setKeys] = useState<CmkKey[]>([]);
  const [active, setActive] = useState<CmkKey | null>(null);
  const [pem, setPem] = useState("");
  const [revoking, setRevoking] = useState<CmkKey | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [hsmConfigs, setHsmConfigs] = useState<HsmConfig[]>([]);
  const [hsmDraft, setHsmDraft] = useState<HsmRegistration>({
    provider_type: "kmip",
    endpoint: "",
    slot_id: "",
    credentials: "",
  });

  const eligible = selectedTenant?.plan === "privacy";

  const reload = useCallback(
    (tid: string) => {
      listKeys(tid).then(setKeys).catch((e: unknown) => setError(String(e)));
      getActiveCmkKey(tid)
        .then(setActive)
        .catch((e: unknown) => setError(String(e)));
      if (eligible) {
        listHsmConfigs(tid)
          .then(setHsmConfigs)
          .catch((e: unknown) => setError(String(e)));
      }
    },
    [eligible],
  );

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onRegister = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId || !pem.trim()) return;
    try {
      await registerCmkKey(selectedTenantId, pem.trim());
      setPem("");
      reload(selectedTenantId);
    } catch (err) {
      setError(String(err));
    }
  };

  const onRotate = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedTenantId || !active || !pem.trim()) return;
    try {
      await rotateCmkKey(selectedTenantId, active.id, pem.trim());
      setPem("");
      reload(selectedTenantId);
    } catch (err) {
      setError(String(err));
    }
  };

  const confirmRevoke = async () => {
    if (!selectedTenantId || !revoking) return;
    try {
      await revokeCmkKey(selectedTenantId, revoking.id);
      setRevoking(null);
      reload(selectedTenantId);
    } catch (err) {
      setError(String(err));
    }
  };

  const onUpload = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => setPem(String(reader.result ?? ""));
    reader.readAsText(file);
  };

  return (
    <section className="kmail-admin-page kmail-cmk">
      <h2>Customer-managed keys</h2>
      <p>
        Privacy-plan tenants can register their own RSA public keys to wrap
        per-tenant DEKs. Keys are stored as PEM with a SHA-256 fingerprint;
        rotation atomically deprecates the previous active key.
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

      {selectedTenantId && !eligible && (
        <p className="kmail-warning">
          This tenant is on the <strong>{selectedTenant?.plan ?? "core"}</strong> plan.
          Customer-managed keys require the <strong>privacy</strong> plan ($9 per
          seat). Upgrade in <em>Pricing &amp; Plans</em> to enable CMK.
        </p>
      )}

      {selectedTenantId && eligible && (
        <div role="tablist" style={{ display: "flex", gap: "0.5rem", marginBottom: "1rem" }}>
          <button role="tab" aria-selected={tab === "pem"} onClick={() => setTab("pem")}>
            PEM key
          </button>
          <button role="tab" aria-selected={tab === "hsm"} onClick={() => setTab("hsm")}>
            HSM (KMIP / PKCS#11)
          </button>
        </div>
      )}

      {info && (
        <p className="kmail-info" role="status">
          {info} <button onClick={() => setInfo(null)}>dismiss</button>
        </p>
      )}

      {selectedTenantId && eligible && tab === "hsm" && (
        <>
          <h3>HSM configurations</h3>
          {hsmConfigs.length === 0 ? (
            <p>No HSM configurations registered yet.</p>
          ) : (
            <table className="kmail-cmk-list">
              <thead>
                <tr>
                  <th>Provider</th>
                  <th>Endpoint</th>
                  <th>Slot</th>
                  <th>Status</th>
                  <th>Last test</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {hsmConfigs.map((c) => (
                  <tr key={c.id}>
                    <td>{c.provider_type}</td>
                    <td><code>{c.endpoint}</code></td>
                    <td>{c.slot_id ?? ""}</td>
                    <td>{c.status}{c.last_test_error ? ` (${c.last_test_error})` : ""}</td>
                    <td>{c.last_test_at ? new Date(c.last_test_at).toLocaleString() : "—"}</td>
                    <td>
                      <button
                        type="button"
                        onClick={async () => {
                          if (!selectedTenantId) return;
                          try {
                            await testHsmConnection(selectedTenantId, c.id);
                            setInfo("Test handshake started.");
                            reload(selectedTenantId);
                          } catch (err) {
                            setError(String(err));
                          }
                        }}
                      >
                        Test
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          <h3>Register HSM</h3>
          <form
            className="kmail-cmk-form"
            onSubmit={async (e) => {
              e.preventDefault();
              if (!selectedTenantId) return;
              try {
                await registerHsmKey(selectedTenantId, hsmDraft);
                setHsmDraft({ provider_type: "kmip", endpoint: "", slot_id: "", credentials: "" });
                setInfo("HSM configuration registered.");
                reload(selectedTenantId);
              } catch (err) {
                setError(String(err));
              }
            }}
          >
            <label>
              Provider
              <select
                value={hsmDraft.provider_type}
                onChange={(e) =>
                  setHsmDraft({ ...hsmDraft, provider_type: e.target.value as "kmip" | "pkcs11" })
                }
              >
                <option value="kmip">KMIP-over-TLS</option>
                <option value="pkcs11">PKCS#11 module</option>
              </select>
            </label>
            <label>
              Endpoint
              <input
                type="text"
                required
                placeholder={
                  hsmDraft.provider_type === "kmip"
                    ? "kmips://hsm.corp.example:5696"
                    : "/usr/lib/softhsm/libsofthsm2.so"
                }
                value={hsmDraft.endpoint}
                onChange={(e) => setHsmDraft({ ...hsmDraft, endpoint: e.target.value })}
              />
            </label>
            <label>
              Slot ID {hsmDraft.provider_type === "kmip" ? "(optional)" : "(required)"}
              <input
                type="text"
                value={hsmDraft.slot_id ?? ""}
                onChange={(e) => setHsmDraft({ ...hsmDraft, slot_id: e.target.value })}
              />
            </label>
            <label>
              Credentials
              <textarea
                rows={3}
                required
                placeholder={
                  hsmDraft.provider_type === "kmip"
                    ? "PEM-encoded mTLS client cert + key"
                    : "PIN"
                }
                value={hsmDraft.credentials}
                onChange={(e) => setHsmDraft({ ...hsmDraft, credentials: e.target.value })}
              />
            </label>
            <button type="submit">Register</button>
          </form>
        </>
      )}

      {selectedTenantId && eligible && tab === "pem" && (
        <>
          <h3>Active key</h3>
          {active ? (
            <table className="kmail-cmk-active">
              <tbody>
                <tr><th>Fingerprint</th><td><code>{active.key_fingerprint}</code></td></tr>
                <tr><th>Algorithm</th><td>{active.algorithm}</td></tr>
                <tr><th>Status</th><td>{active.status}</td></tr>
                <tr><th>Created</th><td>{active.created_at}</td></tr>
              </tbody>
            </table>
          ) : (
            <p>No active key registered.</p>
          )}

          <h3>{active ? "Rotate key" : "Register key"}</h3>
          <form onSubmit={active ? onRotate : onRegister} className="kmail-cmk-form">
            <label>
              PEM public key
              <textarea
                rows={8}
                value={pem}
                onChange={(e) => setPem(e.target.value)}
                placeholder="-----BEGIN PUBLIC KEY-----..."
                required
              />
            </label>
            <label>
              Or upload a .pem file
              <input type="file" accept=".pem,.txt,.crt,application/x-pem-file" onChange={onUpload} />
            </label>
            <button type="submit" disabled={!pem.trim()}>
              {active ? "Rotate" : "Register"}
            </button>
          </form>

          <h3>All keys</h3>
          {keys.length === 0 ? (
            <p>No keys yet.</p>
          ) : (
            <table className="kmail-cmk-list">
              <thead>
                <tr><th>Fingerprint</th><th>Status</th><th>Created</th><th /></tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.id}>
                    <td><code>{k.key_fingerprint}</code></td>
                    <td>{k.status}</td>
                    <td>{k.created_at}</td>
                    <td>
                      {k.status !== "revoked" && (
                        <button type="button" onClick={() => setRevoking(k)}>
                          Revoke
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}

      {revoking && (
        <div className="kmail-modal" role="dialog" aria-modal="true">
          <div className="kmail-modal-content">
            <h3>Revoke key?</h3>
            <p>
              You are about to revoke key <code>{revoking.key_fingerprint}</code>.
              All wrapped DEKs created by this key must be re-wrapped under a
              different active key before this is safe in production.
            </p>
            <div className="kmail-modal-actions">
              <button type="button" onClick={() => setRevoking(null)}>Cancel</button>
              <button type="button" onClick={confirmRevoke}>Confirm revoke</button>
            </div>
          </div>
        </div>
      )}

      {error && <p className="kmail-error">{error}</p>}
    </section>
  );
}

async function listKeys(tid: string): Promise<CmkKey[]> {
  return (await listCmkKeys(tid)) ?? [];
}
