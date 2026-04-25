import { useEffect, useState } from "react";

import {
  getDnsWizardStatus,
  verifyDomain,
  listDomains,
  type DnsWizardStatus,
  type DnsWizardStep,
  type TenantDomain,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

/**
 * DnsWizard walks a tenant admin through publishing the seven DNS
 * records KMail requires (MX → SPF → DKIM → DMARC → MTA-STS →
 * TLS-RPT → autoconfig). Each step renders the expected record,
 * a copy-to-clipboard button, and a live Verify button that calls
 * `POST /api/v1/tenants/{id}/domains/{domainId}/verify`.
 */
export default function DnsWizard() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [domains, setDomains] = useState<TenantDomain[]>([]);
  const [selectedDomainId, setSelectedDomainId] = useState<string>("");
  const [status, setStatus] = useState<DnsWizardStatus | null>(null);
  const [stepIdx, setStepIdx] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [verifying, setVerifying] = useState(false);

  useEffect(() => {
    if (!selectedTenantId) return;
    listDomains(selectedTenantId).then(setDomains).catch((e) => setError(String(e)));
  }, [selectedTenantId]);

  const reload = async () => {
    if (!selectedTenantId || !selectedDomainId) return;
    setLoading(true);
    setError(null);
    try {
      const s = await getDnsWizardStatus(selectedTenantId, selectedDomainId);
      setStatus(s);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!selectedTenantId || !selectedDomainId) return;
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedTenantId, selectedDomainId]);

  const runVerify = async () => {
    if (!selectedTenantId || !selectedDomainId) return;
    setVerifying(true);
    try {
      await verifyDomain(selectedTenantId, selectedDomainId);
      await reload();
    } catch (e) {
      setError(String(e));
    } finally {
      setVerifying(false);
    }
  };

  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // ignore
    }
  };

  const step: DnsWizardStep | undefined = status?.steps[stepIdx];

  return (
    <section className="kmail-admin-page">
      <h2>DNS Wizard</h2>
      <p>Publish MX / SPF / DKIM / DMARC / MTA-STS / TLS-RPT / autoconfig records, then verify.</p>

      <div className="kmail-admin-controls">
        <label>
          Tenant&nbsp;
          <select value={selectedTenantId ?? ""} onChange={(e) => selectTenant(e.target.value)}>
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </label>
        <label>
          Domain&nbsp;
          <select value={selectedDomainId} onChange={(e) => setSelectedDomainId(e.target.value)}>
            <option value="">— select —</option>
            {domains.map((d) => (
              <option key={d.id} value={d.id}>{d.domain}</option>
            ))}
          </select>
        </label>
      </div>

      {error && <p className="kmail-error">{error}</p>}
      {loading && <p>Loading…</p>}

      {status && (
        <div className="kmail-dns-wizard">
          <ol className="kmail-wizard-steps">
            {status.steps.map((s, idx) => (
              <li
                key={s.key}
                className={`kmail-wizard-step ${idx === stepIdx ? "active" : ""} ${s.verified ? "verified" : "pending"}`}
                onClick={() => setStepIdx(idx)}
              >
                <span className="kmail-wizard-step-indicator">
                  {s.verified ? "✓" : idx + 1}
                </span>
                <span>{s.label}</span>
              </li>
            ))}
          </ol>

          {step && (
            <div className="kmail-wizard-body">
              <h3>
                Step {stepIdx + 1} / {status.steps.length}: {step.label}
                <span className={`kmail-wizard-badge ${step.verified ? "ok" : "fail"}`}>
                  {step.verified ? "Verified" : "Not verified"}
                </span>
              </h3>

              {step.record ? (
                <table className="kmail-dns-record">
                  <tbody>
                    <tr><th>Type</th><td>{step.record.type}</td></tr>
                    <tr><th>Name</th><td><code>{step.record.name}</code></td></tr>
                    <tr>
                      <th>Value</th>
                      <td>
                        <code>{step.record.value}</code>
                        <button type="button" onClick={() => copy(step.record!.value)}>Copy</button>
                      </td>
                    </tr>
                    {step.record.ttl !== undefined && (
                      <tr><th>TTL</th><td>{step.record.ttl}</td></tr>
                    )}
                    {step.record.priority !== undefined && step.record.priority > 0 && (
                      <tr><th>Priority</th><td>{step.record.priority}</td></tr>
                    )}
                    {step.record.notes && (
                      <tr><th>Notes</th><td>{step.record.notes}</td></tr>
                    )}
                  </tbody>
                </table>
              ) : (
                <p>KMail is not configured to require this record type.</p>
              )}

              <div className="kmail-wizard-nav">
                <button
                  type="button"
                  onClick={() => setStepIdx(Math.max(0, stepIdx - 1))}
                  disabled={stepIdx === 0}
                >
                  ← Previous
                </button>
                <button type="button" onClick={runVerify} disabled={verifying}>
                  {verifying ? "Verifying…" : "Verify all records"}
                </button>
                <button
                  type="button"
                  onClick={() => setStepIdx(Math.min(status.steps.length - 1, stepIdx + 1))}
                  disabled={stepIdx === status.steps.length - 1}
                >
                  Next →
                </button>
              </div>
            </div>
          )}

          {status.allVerified && (
            <div className="kmail-wizard-summary">
              <h3>All records verified</h3>
              <p>Your domain is ready to send and receive mail through KMail.</p>
            </div>
          )}
          {!status.allVerified && (
            <div className="kmail-wizard-summary">
              <h3>Outstanding records</h3>
              <ul>
                {status.steps.filter((s) => !s.verified).map((s) => (
                  <li key={s.key}>{s.label}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
