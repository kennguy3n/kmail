import { useEffect, useState } from "react";
import { Link } from "react-router-dom";

import {
  createGrant,
  deleteGrant,
  listGrants,
  updateGrant,
} from "../../api/delegation";
import { jmapClient } from "../../api/jmap";
import type { DelegationGrant, DelegationGrantDraft, Identity } from "../../types";

/**
 * Delegate-access / send-as management page.
 *
 * CRUD over the client-side {@link listGrants} store. A grant lets
 * `delegateEmail` read (or read-write) the `ownerEmail` mailbox
 * and/or send under that identity. The "send-as" side is the part
 * with a live JMAP counterpart — Compose surfaces granted owners in
 * its From dropdown via `sendAsOwnersFor` — while the read/write
 * access level is recorded here for an admin/backend to enforce.
 */
const ACCESS_OPTIONS: DelegationGrant["access"][] = [
  "none",
  "read",
  "read-write",
];

const emptyDraft = (ownerEmail: string): DelegationGrantDraft => ({
  ownerEmail,
  delegateEmail: "",
  access: "read",
  sendAs: false,
});

export default function Delegation() {
  const [grants, setGrants] = useState<DelegationGrant[]>(() => listGrants());
  const [identities, setIdentities] = useState<Identity[]>([]);
  const [draft, setDraft] = useState<DelegationGrantDraft>(() => emptyDraft(""));
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    jmapClient
      .getIdentities()
      .then((list) => {
        if (cancelled) return;
        setIdentities(list);
        if (list.length > 0) {
          setDraft((d) => (d.ownerEmail ? d : emptyDraft(list[0].email)));
        }
      })
      .catch(() => {
        // Identities just pre-fill the owner field; the form still
        // works with a manually typed owner email.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const refresh = () => setGrants(listGrants());

  const onCreate = () => {
    setError(null);
    setInfo(null);
    try {
      createGrant(draft);
      setInfo(`Granted ${draft.delegateEmail} access to ${draft.ownerEmail}.`);
      setDraft((d) => ({ ...emptyDraft(d.ownerEmail) }));
      refresh();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  const onChangeAccess = (
    grant: DelegationGrant,
    access: DelegationGrant["access"],
  ) => {
    updateGrant(grant.id, { access, sendAs: grant.sendAs });
    refresh();
  };

  const onToggleSendAs = (grant: DelegationGrant, sendAs: boolean) => {
    updateGrant(grant.id, { access: grant.access, sendAs });
    refresh();
  };

  const onRevoke = (id: string) => {
    deleteGrant(id);
    refresh();
  };

  return (
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Delegate access &amp; Send-as</h2>
        <Link to="/mail" style={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      {error && <div style={styles.error}>{error}</div>}
      {info && <div style={styles.info}>{info}</div>}

      <div style={styles.form}>
        <h3 style={styles.subhead}>Grant access</h3>
        <div style={styles.formGrid}>
          <label style={styles.fieldLabel} htmlFor="del-owner">
            Mailbox owner
          </label>
          {identities.length > 0 ? (
            <select
              id="del-owner"
              value={draft.ownerEmail}
              onChange={(e) =>
                setDraft((d) => ({ ...d, ownerEmail: e.target.value }))
              }
              style={styles.input}
            >
              {identities.map((id) => (
                <option key={id.id} value={id.email}>
                  {id.name ? `${id.name} <${id.email}>` : id.email}
                </option>
              ))}
            </select>
          ) : (
            <input
              id="del-owner"
              type="email"
              value={draft.ownerEmail}
              onChange={(e) =>
                setDraft((d) => ({ ...d, ownerEmail: e.target.value }))
              }
              placeholder="owner@example.com"
              style={styles.input}
            />
          )}

          <label style={styles.fieldLabel} htmlFor="del-delegate">
            Delegate
          </label>
          <input
            id="del-delegate"
            type="email"
            value={draft.delegateEmail}
            onChange={(e) =>
              setDraft((d) => ({ ...d, delegateEmail: e.target.value }))
            }
            placeholder="delegate@example.com"
            style={styles.input}
          />

          <label style={styles.fieldLabel} htmlFor="del-access">
            Access level
          </label>
          <select
            id="del-access"
            value={draft.access}
            onChange={(e) =>
              setDraft((d) => ({
                ...d,
                access: e.target.value as DelegationGrant["access"],
              }))
            }
            style={styles.input}
          >
            {ACCESS_OPTIONS.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>

          <label style={styles.fieldLabel} htmlFor="del-sendas">
            Send-as
          </label>
          <label style={styles.checkboxRow}>
            <input
              id="del-sendas"
              type="checkbox"
              checked={draft.sendAs}
              onChange={(e) =>
                setDraft((d) => ({ ...d, sendAs: e.target.checked }))
              }
            />
            Allow sending under this identity
          </label>
        </div>
        <button type="button" onClick={onCreate} style={styles.primaryButton}>
          Add grant
        </button>
      </div>

      <h3 style={styles.subhead}>Existing grants</h3>
      {grants.length === 0 ? (
        <p style={styles.muted}>No delegation grants yet.</p>
      ) : (
        <table style={styles.table}>
          <thead>
            <tr>
              <th style={styles.th}>Owner</th>
              <th style={styles.th}>Delegate</th>
              <th style={styles.th}>Access</th>
              <th style={styles.th}>Send-as</th>
              <th style={styles.th} aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {grants.map((g) => (
              <tr key={g.id}>
                <td style={styles.td}>{g.ownerEmail}</td>
                <td style={styles.td}>{g.delegateEmail}</td>
                <td style={styles.td}>
                  <select
                    value={g.access}
                    onChange={(e) =>
                      onChangeAccess(
                        g,
                        e.target.value as DelegationGrant["access"],
                      )
                    }
                    aria-label={`Access for ${g.delegateEmail}`}
                  >
                    {ACCESS_OPTIONS.map((a) => (
                      <option key={a} value={a}>
                        {a}
                      </option>
                    ))}
                  </select>
                </td>
                <td style={styles.td}>
                  <input
                    type="checkbox"
                    checked={g.sendAs}
                    onChange={(e) => onToggleSendAs(g, e.target.checked)}
                    aria-label={`Send-as for ${g.delegateEmail}`}
                  />
                </td>
                <td style={styles.td}>
                  <button
                    type="button"
                    onClick={() => onRevoke(g.id)}
                    style={styles.dangerButton}
                  >
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

const styles: Record<string, React.CSSProperties> = {
  root: { padding: "1rem", maxWidth: "860px" },
  header: {
    display: "flex",
    alignItems: "baseline",
    justifyContent: "space-between",
    marginBottom: "1rem",
  },
  title: { margin: 0, fontSize: "1.25rem" },
  backLink: { color: "#2563eb", textDecoration: "none", fontSize: "0.9rem" },
  subhead: { fontSize: "1rem", margin: "1rem 0 0.5rem" },
  form: {
    border: "1px solid #e5e7eb",
    borderRadius: "0.5rem",
    padding: "1rem",
    background: "#f9fafb",
  },
  formGrid: {
    display: "grid",
    gridTemplateColumns: "140px 1fr",
    gap: "0.5rem 0.75rem",
    alignItems: "center",
    marginBottom: "0.75rem",
  },
  fieldLabel: { fontSize: "0.8rem", fontWeight: 600, color: "#374151" },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  checkboxRow: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.85rem",
    color: "#374151",
  },
  primaryButton: {
    padding: "0.45rem 0.9rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  dangerButton: {
    padding: "0.3rem 0.6rem",
    background: "#fff",
    color: "#991b1b",
    border: "1px solid #fca5a5",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.8rem",
  },
  table: { width: "100%", borderCollapse: "collapse", fontSize: "0.85rem" },
  th: {
    textAlign: "left",
    padding: "0.4rem 0.5rem",
    borderBottom: "2px solid #e5e7eb",
    color: "#374151",
  },
  td: { padding: "0.4rem 0.5rem", borderBottom: "1px solid #f3f4f6" },
  error: {
    padding: "0.4rem 0.6rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
    marginBottom: "0.5rem",
  },
  info: {
    padding: "0.4rem 0.6rem",
    background: "#ecfdf5",
    color: "#065f46",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
    marginBottom: "0.5rem",
  },
  muted: { color: "#6b7280", fontStyle: "italic", fontSize: "0.85rem" },
};
