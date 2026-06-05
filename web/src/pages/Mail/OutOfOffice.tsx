import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";

import {
  createSieveRule,
  deploySieveRules,
  listSieveRules,
  updateSieveRule,
  type SieveRule,
} from "../../api/admin";
import { buildVacationScript, VACATION_RULE_NAME } from "../../api/sieve";
import { useTenantSelection } from "../Admin/useTenantSelection";
import type { VacationSettings } from "../../types";

/**
 * Out-of-Office / vacation auto-reply editor.
 *
 * Reads/writes a single well-known Sieve rule (named
 * {@link VACATION_RULE_NAME}) on the selected tenant via the
 * existing admin Sieve CRUD. The editor keeps a normalized
 * {@link VacationSettings} shape and generates the RFC 5230
 * `vacation` script through {@link buildVacationScript} on save, so
 * the user never edits Sieve by hand. Toggling "enabled" flips the
 * underlying rule's `enabled` flag and (on enable) regenerates the
 * script from the current message/date range.
 */
const defaultSettings = (): VacationSettings => ({
  enabled: false,
  subject: "Out of Office",
  message: "I am currently out of the office and will reply on my return.",
  startDate: null,
  endDate: null,
  contactsOnly: false,
});

export default function OutOfOffice() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [settings, setSettings] = useState<VacationSettings>(defaultSettings);
  const [existingRule, setExistingRule] = useState<SieveRule | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);

  const reload = useCallback((tenantId: string) => {
    setLoading(true);
    setError(null);
    listSieveRules(tenantId)
      .then(({ rules }) => {
        const rule =
          rules.find((r) => r.name === VACATION_RULE_NAME) ?? null;
        setExistingRule(rule);
        if (rule) {
          // We can't fully round-trip Sieve back into the editor, so
          // keep the editable message/dates in component state and
          // only adopt the rule's enabled flag from the server. The
          // message persists across reloads within a session; a
          // fresh load shows the default copy with the live toggle.
          setSettings((s) => ({ ...s, enabled: rule.enabled }));
        }
      })
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const persist = async (next: VacationSettings) => {
    if (!selectedTenantId) {
      setError("Select a tenant first.");
      return;
    }
    setSaving(true);
    setError(null);
    setInfo(null);
    try {
      const script = buildVacationScript(next);
      if (existingRule) {
        const updated = await updateSieveRule(selectedTenantId, {
          ...existingRule,
          script,
          enabled: next.enabled,
        });
        setExistingRule(updated);
      } else {
        const created = await createSieveRule(selectedTenantId, {
          name: VACATION_RULE_NAME,
          script,
          priority: 100,
          enabled: next.enabled,
          user_id: null,
        });
        setExistingRule(created);
      }
      await deploySieveRules(selectedTenantId);
      setInfo(
        next.enabled
          ? "Auto-reply is on and deployed."
          : "Auto-reply saved and turned off.",
      );
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };

  const onToggle = (enabled: boolean) => {
    const next = { ...settings, enabled };
    setSettings(next);
    void persist(next);
  };

  const preview = buildVacationScript(settings);

  return (
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Out of Office</h2>
        <Link to="/mail" style={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div style={styles.tenantRow}>
        <label htmlFor="ooo-tenant" style={styles.fieldLabel}>
          Tenant
        </label>
        <select
          id="ooo-tenant"
          value={selectedTenantId ?? ""}
          onChange={(e) => selectTenant(e.target.value)}
          style={styles.input}
        >
          <option value="">— select —</option>
          {(tenants ?? []).map((t) => (
            <option key={t.id} value={t.id}>
              {t.name ?? t.id}
            </option>
          ))}
        </select>
      </div>

      {error && <div style={styles.error}>{error}</div>}
      {info && <div style={styles.info}>{info}</div>}
      {loading && <p style={styles.muted}>Loading…</p>}

      <div style={styles.statusRow}>
        <span
          style={{
            ...styles.statusDot,
            background: settings.enabled ? "#22c55e" : "#9ca3af",
          }}
          aria-hidden="true"
        />
        <strong>{settings.enabled ? "Auto-reply is ON" : "Auto-reply is OFF"}</strong>
        <label style={styles.switch}>
          <input
            type="checkbox"
            checked={settings.enabled}
            disabled={!selectedTenantId || saving}
            onChange={(e) => onToggle(e.target.checked)}
          />
          {settings.enabled ? "Turn off" : "Turn on"}
        </label>
      </div>

      <label style={styles.fieldLabel} htmlFor="ooo-subject">
        Reply subject
      </label>
      <input
        id="ooo-subject"
        type="text"
        value={settings.subject}
        onChange={(e) =>
          setSettings((s) => ({ ...s, subject: e.target.value }))
        }
        style={styles.input}
      />

      <label style={styles.fieldLabel} htmlFor="ooo-message">
        Reply message
      </label>
      <textarea
        id="ooo-message"
        value={settings.message}
        onChange={(e) =>
          setSettings((s) => ({ ...s, message: e.target.value }))
        }
        rows={5}
        style={styles.textarea}
      />

      <div style={styles.dateRow}>
        <div>
          <label style={styles.fieldLabel} htmlFor="ooo-start">
            Start date (optional)
          </label>
          <input
            id="ooo-start"
            type="date"
            value={settings.startDate ?? ""}
            onChange={(e) =>
              setSettings((s) => ({
                ...s,
                startDate: e.target.value || null,
              }))
            }
            style={styles.input}
          />
        </div>
        <div>
          <label style={styles.fieldLabel} htmlFor="ooo-end">
            End date (optional)
          </label>
          <input
            id="ooo-end"
            type="date"
            value={settings.endDate ?? ""}
            onChange={(e) =>
              setSettings((s) => ({ ...s, endDate: e.target.value || null }))
            }
            style={styles.input}
          />
        </div>
      </div>

      <label style={styles.checkboxRow}>
        <input
          type="checkbox"
          checked={settings.contactsOnly}
          onChange={(e) =>
            setSettings((s) => ({ ...s, contactsOnly: e.target.checked }))
          }
        />
        Only reply to senders in my contacts
      </label>

      <div style={styles.buttonRow}>
        <button
          type="button"
          onClick={() => void persist(settings)}
          disabled={!selectedTenantId || saving}
          style={styles.primaryButton}
        >
          {saving ? "Saving…" : "Save"}
        </button>
      </div>

      <details style={styles.preview}>
        <summary style={styles.previewSummary}>Generated Sieve script</summary>
        <pre style={styles.previewPre}>{preview}</pre>
      </details>
    </section>
  );
}

const styles: Record<string, React.CSSProperties> = {
  root: { padding: "1rem", maxWidth: "720px", display: "grid", gap: "0.5rem" },
  header: {
    display: "flex",
    alignItems: "baseline",
    justifyContent: "space-between",
    marginBottom: "0.5rem",
  },
  title: { margin: 0, fontSize: "1.25rem" },
  backLink: { color: "#2563eb", textDecoration: "none", fontSize: "0.9rem" },
  tenantRow: { display: "grid", gap: "0.25rem", marginBottom: "0.5rem" },
  statusRow: {
    display: "flex",
    alignItems: "center",
    gap: "0.6rem",
    padding: "0.6rem 0.75rem",
    background: "#f9fafb",
    border: "1px solid #e5e7eb",
    borderRadius: "0.375rem",
    margin: "0.25rem 0 0.75rem",
  },
  statusDot: {
    width: "0.7rem",
    height: "0.7rem",
    borderRadius: "999px",
    display: "inline-block",
  },
  switch: {
    marginLeft: "auto",
    display: "flex",
    alignItems: "center",
    gap: "0.35rem",
    fontSize: "0.85rem",
    cursor: "pointer",
  },
  fieldLabel: { fontSize: "0.8rem", fontWeight: 600, color: "#374151" },
  input: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    width: "100%",
    boxSizing: "border-box",
  },
  textarea: {
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    width: "100%",
    boxSizing: "border-box",
    resize: "vertical",
  },
  dateRow: {
    display: "grid",
    gridTemplateColumns: "1fr 1fr",
    gap: "0.75rem",
  },
  checkboxRow: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    fontSize: "0.85rem",
    color: "#374151",
    marginTop: "0.25rem",
  },
  buttonRow: { display: "flex", gap: "0.5rem", marginTop: "0.5rem" },
  primaryButton: {
    padding: "0.45rem 0.9rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
    fontSize: "0.85rem",
  },
  error: {
    padding: "0.4rem 0.6rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
  },
  info: {
    padding: "0.4rem 0.6rem",
    background: "#ecfdf5",
    color: "#065f46",
    borderRadius: "0.25rem",
    fontSize: "0.85rem",
  },
  muted: { color: "#6b7280", fontStyle: "italic", fontSize: "0.85rem" },
  preview: { marginTop: "0.75rem" },
  previewSummary: {
    cursor: "pointer",
    fontSize: "0.85rem",
    color: "#374151",
  },
  previewPre: {
    background: "#0f172a",
    color: "#e2e8f0",
    padding: "0.75rem",
    borderRadius: "0.375rem",
    overflowX: "auto",
    fontSize: "0.8rem",
  },
};
