import { useEffect, useState } from "react";

import {
  cancelMigrationJob,
  createMigrationJob,
  listMigrationJobs,
  pauseMigrationJob,
  resumeMigrationJob,
  testMigrationConnection,
  type CreateMigrationJobInput,
  type MigrationJob,
} from "../../api/admin";

import { useTenantSelection } from "./useTenantSelection";

const PROVIDER_DEFAULTS: Record<CreateMigrationJobInput["source_type"], { host: string; port: number }> = {
  gmail_imap: { host: "imap.gmail.com", port: 993 },
  generic_imap: { host: "", port: 993 },
  ms365_imap: { host: "outlook.office365.com", port: 993 },
};

/**
 * MigrationAdmin is the tenant-side wizard for onboarding an
 * existing mailbox into KMail. It drives the Go migration
 * orchestrator at `internal/migration/`.
 */
export default function MigrationAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [jobs, setJobs] = useState<MigrationJob[]>([]);
  const [draft, setDraft] = useState<CreateMigrationJobInput>({
    source_type: "gmail_imap",
    source_host: PROVIDER_DEFAULTS.gmail_imap.host,
    source_port: PROVIDER_DEFAULTS.gmail_imap.port,
    source_user: "",
    source_password: "",
    destination_user_id: "",
  });
  const [step, setStep] = useState<1 | 2 | 3>(1);
  const [error, setError] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<
    { ok: true } | { ok: false; error: string } | null
  >(null);
  const [testing, setTesting] = useState(false);

  const runTestConnection = async () => {
    if (!selectedTenantId) return;
    setTesting(true);
    setTestResult(null);
    try {
      const res = await testMigrationConnection(selectedTenantId, {
        host: draft.source_host,
        port: draft.source_port ?? 993,
        username: draft.source_user,
        password: draft.source_password,
        use_tls: (draft.source_port ?? 993) === 993,
      });
      if (res.ok) {
        setTestResult({ ok: true });
      } else {
        setTestResult({ ok: false, error: res.error ?? "unknown error" });
      }
    } catch (e) {
      setTestResult({ ok: false, error: String(e) });
    } finally {
      setTesting(false);
    }
  };

  const reload = async () => {
    if (!selectedTenantId) return;
    try {
      const rows = await listMigrationJobs(selectedTenantId);
      setJobs(rows);
    } catch (e) {
      setError(String(e));
    }
  };

  useEffect(() => {
    void reload();
    // The previous version closed over the initial empty `jobs`
    // array, so the running-job check was always false and the
    // poll never fired. Just reload unconditionally — `reload`
    // is a no-op when no tenant is selected.
    const id = setInterval(() => {
      void reload();
    }, 5000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedTenantId]);

  // updateCredential mutates draft fields that feed into the
  // "Test connection" probe (host / port / user / password) and
  // clears any prior green/red banner so the operator never sees
  // a stale success indicator after editing credentials.
  const updateCredential = (patch: Partial<CreateMigrationJobInput>) => {
    setDraft({ ...draft, ...patch });
    setTestResult(null);
  };

  const pickProvider = (source_type: CreateMigrationJobInput["source_type"]) => {
    const d = PROVIDER_DEFAULTS[source_type];
    updateCredential({ source_type, source_host: d.host, source_port: d.port });
  };

  const submit = async () => {
    if (!selectedTenantId) return;
    try {
      await createMigrationJob(selectedTenantId, draft);
      setStep(1);
      await reload();
    } catch (e) {
      setError(String(e));
    }
  };

  const completed = jobs.find((j) => j.status === "completed");

  return (
    <section className="kmail-admin-page">
      <h2>Migration wizard</h2>
      {error && <p className="kmail-error">{error}</p>}

      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select value={selectedTenantId ?? ""} onChange={(e) => selectTenant(e.target.value)}>
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
          </select>
        </label>
      </div>

      <ol className="kmail-wizard-steps">
        {[1, 2, 3].map((n) => (
          <li key={n} className={`kmail-wizard-step ${step === n ? "active" : ""}`} onClick={() => setStep(n as 1 | 2 | 3)}>
            <span className="kmail-wizard-step-indicator">{n}</span>
            <span>{n === 1 ? "Source" : n === 2 ? "Credentials" : "Confirm"}</span>
          </li>
        ))}
      </ol>

      {step === 1 && (
        <div>
          <h3>Step 1: Source</h3>
          {(Object.keys(PROVIDER_DEFAULTS) as CreateMigrationJobInput["source_type"][]).map((k) => (
            <label key={k}>
              <input
                type="radio"
                checked={draft.source_type === k}
                onChange={() => pickProvider(k)}
              />
              {k.replace(/_/g, " ")}
            </label>
          ))}
          <button type="button" onClick={() => setStep(2)}>Next →</button>
        </div>
      )}

      {step === 2 && (
        <div>
          <h3>Step 2: Credentials</h3>
          <label>
            Host
            <input value={draft.source_host} onChange={(e) => updateCredential({ source_host: e.target.value })} />
          </label>
          <label>
            Port
            <input
              type="number"
              value={draft.source_port ?? 993}
              onChange={(e) => updateCredential({ source_port: Number(e.target.value) })}
            />
          </label>
          <label>
            Source user
            <input value={draft.source_user} onChange={(e) => updateCredential({ source_user: e.target.value })} />
          </label>
          <label>
            Source password
            <input
              type="password"
              value={draft.source_password}
              onChange={(e) => updateCredential({ source_password: e.target.value })}
            />
          </label>
          <label>
            Destination user
            <input value={draft.destination_user_id} onChange={(e) => setDraft({ ...draft, destination_user_id: e.target.value })} />
          </label>
          <div className="kmail-wizard-actions">
            <button
              type="button"
              onClick={runTestConnection}
              disabled={
                testing ||
                !selectedTenantId ||
                !draft.source_host ||
                !draft.source_user ||
                !draft.source_password
              }
            >
              {testing ? "Testing…" : "Test connection"}
            </button>
            {testResult?.ok === true && (
              <span className="kmail-success">IMAP login succeeded.</span>
            )}
            {testResult?.ok === false && (
              <span className="kmail-error">Connection failed: {testResult.error}</span>
            )}
          </div>
          <button type="button" onClick={() => setStep(1)}>← Back</button>
          <button type="button" onClick={() => setStep(3)}>Next →</button>
        </div>
      )}

      {step === 3 && (
        <div>
          <h3>Step 3: Confirm</h3>
          <pre>{JSON.stringify({ ...draft, source_password: "***" }, null, 2)}</pre>
          <button type="button" onClick={() => setStep(2)}>← Back</button>
          <button type="button" onClick={submit}>Start migration</button>
        </div>
      )}

      <h3>Jobs</h3>
      <table className="kmail-admin-table">
        <thead>
          <tr>
            <th>Source</th>
            <th>User</th>
            <th>Status</th>
            <th>Progress</th>
            <th>Started</th>
            <th>Completed</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {jobs.map((j) => {
            const pct = j.messages_total > 0
              ? Math.round((j.messages_synced / j.messages_total) * 100)
              : 0;
            return (
              <tr key={j.id}>
                <td>{j.source_type}</td>
                <td>{j.source_user}</td>
                <td>{j.status}</td>
                <td>{pct}% ({j.messages_synced.toLocaleString()}/{j.messages_total.toLocaleString()})</td>
                <td>{j.started_at ? new Date(j.started_at).toLocaleString() : "—"}</td>
                <td>{j.completed_at ? new Date(j.completed_at).toLocaleString() : "—"}</td>
                <td>
                  {j.status === "running" && selectedTenantId && (
                    <button type="button" onClick={() => pauseMigrationJob(selectedTenantId, j.id).then(reload)}>Pause</button>
                  )}
                  {j.status === "paused" && selectedTenantId && (
                    <button type="button" onClick={() => resumeMigrationJob(selectedTenantId, j.id).then(reload)}>Resume</button>
                  )}
                  {(j.status === "running" || j.status === "paused") && selectedTenantId && (
                    <button type="button" onClick={() => cancelMigrationJob(selectedTenantId, j.id).then(reload)}>Cancel</button>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>

      {completed && (
        <div className="kmail-wizard-summary">
          <h3>Cutover checklist</h3>
          <ol>
            <li>Update your authoritative DNS MX records to KMail's MX host.</li>
            <li>Verify DNS propagation with the DNS wizard.</li>
            <li>Send + receive a test message through the new mailbox.</li>
            <li>Update client configurations (IMAP/SMTP/autoconfig).</li>
            <li>Archive or disable the legacy mailbox.</li>
          </ol>
        </div>
      )}
    </section>
  );
}
