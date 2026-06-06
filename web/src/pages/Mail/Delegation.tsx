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
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Delegate access &amp; Send-as</h2>
        <Link to="/mail" className={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      {error && <div className={styles.error}>{error}</div>}
      {info && <div className={styles.info}>{info}</div>}

      <div className={styles.form}>
        <h3 className={styles.subhead}>Grant access</h3>
        <div className={styles.formGrid}>
          <label className={styles.fieldLabel} htmlFor="del-owner">
            Mailbox owner
          </label>
          {identities.length > 0 ? (
            <select
              id="del-owner"
              value={draft.ownerEmail}
              onChange={(e) =>
                setDraft((d) => ({ ...d, ownerEmail: e.target.value }))
              }
              className={styles.input}
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
              className={styles.input}
            />
          )}

          <label className={styles.fieldLabel} htmlFor="del-delegate">
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
            className={styles.input}
          />

          <label className={styles.fieldLabel} htmlFor="del-access">
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
            className={styles.input}
          >
            {ACCESS_OPTIONS.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>

          <label className={styles.fieldLabel} htmlFor="del-sendas">
            Send-as
          </label>
          <label className={styles.checkboxRow}>
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
        <button type="button" onClick={onCreate} className={styles.primaryButton}>
          Add grant
        </button>
      </div>

      <h3 className={styles.subhead}>Existing grants</h3>
      {grants.length === 0 ? (
        <p className={styles.muted}>No delegation grants yet.</p>
      ) : (
        <table className={styles.table}>
          <thead>
            <tr>
              <th className={styles.th}>Owner</th>
              <th className={styles.th}>Delegate</th>
              <th className={styles.th}>Access</th>
              <th className={styles.th}>Send-as</th>
              <th className={styles.th} aria-label="Actions" />
            </tr>
          </thead>
          <tbody>
            {grants.map((g) => (
              <tr key={g.id}>
                <td className={styles.td}>{g.ownerEmail}</td>
                <td className={styles.td}>{g.delegateEmail}</td>
                <td className={styles.td}>
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
                <td className={styles.td}>
                  <input
                    type="checkbox"
                    checked={g.sendAs}
                    onChange={(e) => onToggleSendAs(g, e.target.checked)}
                    aria-label={`Send-as for ${g.delegateEmail}`}
                  />
                </td>
                <td className={styles.td}>
                  <button
                    type="button"
                    onClick={() => onRevoke(g.id)}
                    className={styles.dangerButton}
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

/** Theme-aware Tailwind class recipes for the Delegation admin view. */
const styles: Record<string, string> = {
  root: "max-w-[860px] p-4",
  header: "mb-4 flex items-baseline justify-between",
  title: "m-0 text-xl font-semibold",
  backLink: "text-sm text-primary no-underline hover:underline",
  subhead: "mb-2 mt-4 text-base font-semibold",
  form: "rounded-lg border border-border bg-surface-muted p-4",
  formGrid: "mb-3 grid grid-cols-[140px_1fr] items-center gap-x-3 gap-y-2",
  fieldLabel: "text-xs font-semibold text-fg-muted",
  input:
    "rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  checkboxRow: "flex items-center gap-1.5 text-sm text-fg-muted",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3.5 py-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  dangerButton:
    "cursor-pointer rounded-md border border-danger/40 bg-surface px-2.5 py-1.5 text-xs text-danger-fg transition-colors hover:bg-danger-bg",
  table: "w-full border-collapse text-sm",
  th: "border-b-2 border-border px-2 py-1.5 text-left text-fg-muted",
  td: "border-b border-border px-2 py-1.5",
  error: "mb-2 rounded-md bg-danger-bg px-2.5 py-1.5 text-sm text-danger-fg",
  info: "mb-2 rounded-md bg-success-bg px-2.5 py-1.5 text-sm text-success-fg",
  muted: "text-sm italic text-fg-muted",
};
