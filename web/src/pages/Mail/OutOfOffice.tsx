import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";

import { cn } from "../../lib/cn";

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
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Out of Office</h2>
        <Link to="/mail" className={styles.backLink}>
          ← Back to mail
        </Link>
      </header>

      <div className={styles.tenantRow}>
        <label htmlFor="ooo-tenant" className={styles.fieldLabel}>
          Tenant
        </label>
        <select
          id="ooo-tenant"
          value={selectedTenantId ?? ""}
          onChange={(e) => selectTenant(e.target.value)}
          className={styles.input}
        >
          <option value="">— select —</option>
          {(tenants ?? []).map((t) => (
            <option key={t.id} value={t.id}>
              {t.name ?? t.id}
            </option>
          ))}
        </select>
      </div>

      {error && <div className={styles.error}>{error}</div>}
      {info && <div className={styles.info}>{info}</div>}
      {loading && <p className={styles.muted}>Loading…</p>}

      <div className={styles.statusRow}>
        <span
          className={cn(
            styles.statusDot,
            settings.enabled ? "bg-success" : "bg-fg-subtle",
          )}
          aria-hidden="true"
        />
        <strong>{settings.enabled ? "Auto-reply is ON" : "Auto-reply is OFF"}</strong>
        <label className={styles.switch}>
          <input
            type="checkbox"
            checked={settings.enabled}
            disabled={!selectedTenantId || saving}
            onChange={(e) => onToggle(e.target.checked)}
          />
          {settings.enabled ? "Turn off" : "Turn on"}
        </label>
      </div>

      <label className={styles.fieldLabel} htmlFor="ooo-subject">
        Reply subject
      </label>
      <input
        id="ooo-subject"
        type="text"
        value={settings.subject}
        onChange={(e) =>
          setSettings((s) => ({ ...s, subject: e.target.value }))
        }
        className={styles.input}
      />

      <label className={styles.fieldLabel} htmlFor="ooo-message">
        Reply message
      </label>
      <textarea
        id="ooo-message"
        value={settings.message}
        onChange={(e) =>
          setSettings((s) => ({ ...s, message: e.target.value }))
        }
        rows={5}
        className={styles.textarea}
      />

      <div className={styles.dateRow}>
        <div>
          <label className={styles.fieldLabel} htmlFor="ooo-start">
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
            className={styles.input}
          />
        </div>
        <div>
          <label className={styles.fieldLabel} htmlFor="ooo-end">
            End date (optional)
          </label>
          <input
            id="ooo-end"
            type="date"
            value={settings.endDate ?? ""}
            onChange={(e) =>
              setSettings((s) => ({ ...s, endDate: e.target.value || null }))
            }
            className={styles.input}
          />
        </div>
      </div>

      <label className={styles.checkboxRow}>
        <input
          type="checkbox"
          checked={settings.contactsOnly}
          onChange={(e) =>
            setSettings((s) => ({ ...s, contactsOnly: e.target.checked }))
          }
        />
        Only reply to senders in my contacts
      </label>

      <div className={styles.buttonRow}>
        <button
          type="button"
          onClick={() => void persist(settings)}
          disabled={!selectedTenantId || saving}
          className={styles.primaryButton}
        >
          {saving ? "Saving…" : "Save"}
        </button>
      </div>

      <details className={styles.preview}>
        <summary className={styles.previewSummary}>Generated Sieve script</summary>
        <pre className={styles.previewPre}>{preview}</pre>
      </details>
    </section>
  );
}

/** Theme-aware Tailwind class recipes for the Out-of-office settings. */
const styles: Record<string, string> = {
  root: "grid max-w-[720px] gap-2 p-4",
  header: "mb-2 flex items-baseline justify-between",
  title: "m-0 text-xl font-semibold",
  backLink: "text-sm text-primary no-underline hover:underline",
  tenantRow: "mb-2 grid gap-1",
  statusRow:
    "mb-3 mt-1 flex items-center gap-2.5 rounded-md border border-border bg-surface-muted px-3 py-2.5",
  statusDot: "inline-block size-3 rounded-pill",
  switch: "ml-auto flex cursor-pointer items-center gap-1.5 text-sm",
  fieldLabel: "text-xs font-semibold text-fg-muted",
  input:
    "box-border w-full rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  textarea:
    "box-border w-full resize-y rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  dateRow: "grid grid-cols-2 gap-3",
  checkboxRow: "mt-1 flex items-center gap-1.5 text-sm text-fg-muted",
  buttonRow: "mt-2 flex gap-2",
  primaryButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3.5 py-1.5 text-sm font-medium text-primary-fg transition-colors hover:bg-primary-hover",
  error: "rounded-md bg-danger-bg px-2.5 py-1.5 text-sm text-danger-fg",
  info: "rounded-md bg-success-bg px-2.5 py-1.5 text-sm text-success-fg",
  muted: "text-sm italic text-fg-muted",
  preview: "mt-3",
  previewSummary: "cursor-pointer text-sm text-fg-muted",
  previewPre:
    "mt-2 overflow-x-auto rounded-md bg-slate-900 p-3 text-xs text-slate-200",
};
