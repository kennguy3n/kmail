/**
 * Guided onboarding checklist for new tenants. Walks an admin
 * through the 8 steps required to get the tenant fully operational.
 */

import { useCallback, useEffect, useState } from "react";

import {
  getOnboardingChecklist,
  skipOnboardingStep,
  unskipOnboardingStep,
  type OnboardingChecklist as Checklist,
  type OnboardingStep,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

export default function OnboardingChecklist() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [checklist, setChecklist] = useState<Checklist | null>(null);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    getOnboardingChecklist(tid).then(setChecklist).catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const total = checklist?.steps.length ?? 0;
  const completed = checklist?.steps.filter((s) => s.status === "complete").length ?? 0;
  const skipped = checklist?.steps.filter((s) => s.status === "skipped").length ?? 0;
  const pct = total === 0 ? 0 : Math.round(((completed + skipped) / total) * 100);

  const onSkip = async (step: OnboardingStep) => {
    if (!selectedTenantId) return;
    const fn = step.status === "skipped" ? unskipOnboardingStep : skipOnboardingStep;
    try {
      await fn(selectedTenantId, step.id);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const statusIcon = (s: OnboardingStep): string => {
    if (s.status === "complete") return "✓";
    if (s.status === "skipped") return "⤳";
    return "○";
  };

  return (
    <section className="kmail-admin-page">
      <h2>Onboarding checklist</h2>

      <div className="kmail-admin-controls">
        <label>
          Tenant
          <select value={selectedTenantId ?? ""} onChange={(e) => selectTenant(e.target.value)}>
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>{t.name}</option>
            ))}
          </select>
        </label>
      </div>

      {error && <p className="kmail-error">{error}</p>}

      {checklist && (
        <>
          <div className="kmail-admin-card">
            <strong>Progress: {pct}%</strong>{" "}
            ({completed} complete, {skipped} skipped, {total - completed - skipped} pending)
            <div style={{
              background: "#eee",
              borderRadius: 4,
              marginTop: 8,
              height: 12,
              overflow: "hidden",
            }}>
              <div style={{
                width: `${pct}%`,
                background: "var(--kmail-accent, #4a8)",
                height: "100%",
              }} />
            </div>
          </div>

          <div style={{ display: "grid", gap: "1rem" }}>
            {checklist.steps.map((s) => (
              <div key={s.id} className="kmail-admin-card">
                <h3>
                  <span aria-hidden style={{ marginRight: 8 }}>{statusIcon(s)}</span>
                  {s.title}{s.optional ? <small> (optional)</small> : null}
                </h3>
                <p>{s.description}</p>
                <div style={{ display: "flex", gap: "0.5rem" }}>
                  {s.link && s.status !== "complete" && (
                    <a href={s.link} className="kmail-button">Open</a>
                  )}
                  {s.optional && s.status !== "complete" && (
                    <button onClick={() => onSkip(s)}>
                      {s.status === "skipped" ? "Unskip" : "Skip"}
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  );
}
