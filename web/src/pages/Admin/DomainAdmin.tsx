import { Fragment, useEffect, useState } from "react";
import type { FormEvent } from "react";
import {
  createDomain,
  getDnsRecords,
  listDomains,
  verifyDomain,
} from "../../api/admin";
import type { DnsRecord, Domain } from "../../types";

const DEV_TENANT_ID = "00000000-0000-0000-0000-000000000000";

function resolveTenantId(): string {
  const params = new URLSearchParams(window.location.search);
  return params.get("tenantId") ?? DEV_TENANT_ID;
}

function badge(ok: boolean, label: string) {
  return (
    <span
      style={{
        padding: "2px 6px",
        borderRadius: 4,
        fontSize: "0.75em",
        marginRight: 4,
        background: ok ? "#e6f4ea" : "#fde7e9",
        color: ok ? "#137333" : "#a50e0e",
      }}
    >
      {label}: {ok ? "OK" : "✗"}
    </span>
  );
}

export default function DomainAdmin() {
  const [tenantId] = useState(resolveTenantId);
  const [domains, setDomains] = useState<Domain[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [newDomain, setNewDomain] = useState("");
  const [busy, setBusy] = useState(false);
  const [records, setRecords] = useState<Record<string, DnsRecord[]>>({});

  async function reload() {
    try {
      setError(null);
      setDomains(await listDomains(tenantId));
    } catch (err) {
      setError((err as Error).message);
    }
  }
  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tenantId]);

  async function onAdd(e: FormEvent) {
    e.preventDefault();
    if (!newDomain.trim()) return;
    setBusy(true);
    try {
      await createDomain(tenantId, { domain: newDomain.trim() });
      setNewDomain("");
      await reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onVerify(domainId: string) {
    setBusy(true);
    try {
      await verifyDomain(tenantId, domainId);
      await reload();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function onShowRecords(domainId: string) {
    try {
      const r = await getDnsRecords(tenantId, domainId);
      setRecords((prev) => ({ ...prev, [domainId]: r }));
    } catch (err) {
      setError((err as Error).message);
    }
  }

  return (
    <section>
      <h2>Domain admin</h2>
      <p style={{ fontSize: "0.9em", color: "#666" }}>Tenant ID: {tenantId}</p>
      {error && <div style={{ color: "crimson" }}>Error: {error}</div>}

      <form onSubmit={onAdd} style={{ marginBottom: 16 }}>
        <input
          type="text"
          placeholder="example.com"
          value={newDomain}
          onChange={(e) => setNewDomain(e.target.value)}
          required
        />{" "}
        <button type="submit" disabled={busy}>
          Add domain
        </button>
      </form>

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #ddd" }}>
            <th>Domain</th>
            <th>Verification</th>
            <th>Verified at</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {domains.map((d) => (
            <Fragment key={d.id}>
              <tr
                style={{ borderBottom: "1px solid #eee", verticalAlign: "top" }}
              >
                <td>{d.domain}</td>
                <td>
                  {badge(d.mxVerified, "MX")}
                  {badge(d.spfVerified, "SPF")}
                  {badge(d.dkimVerified, "DKIM")}
                  {badge(d.dmarcVerified, "DMARC")}
                </td>
                <td>{d.verifiedAt ? new Date(d.verifiedAt).toLocaleString() : "—"}</td>
                <td>
                  <button onClick={() => onVerify(d.id)} disabled={busy}>
                    Verify
                  </button>{" "}
                  <button onClick={() => onShowRecords(d.id)}>Records</button>
                </td>
              </tr>
              {records[d.id] && (
                <tr>
                  <td colSpan={4}>
                    <pre style={{ background: "#f7f7f7", padding: 8 }}>
                      {records[d.id]
                        .map(
                          (r) =>
                            `${r.type}\t${r.host}\t${r.value}${
                              r.priority ? `\t(priority ${r.priority})` : ""
                            }`,
                        )
                        .join("\n")}
                    </pre>
                  </td>
                </tr>
              )}
            </Fragment>
          ))}
          {domains.length === 0 && (
            <tr>
              <td colSpan={4} style={{ color: "#777", padding: 12 }}>
                No domains configured.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </section>
  );
}
