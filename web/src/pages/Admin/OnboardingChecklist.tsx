/**
 * Guided onboarding checklist for new tenants. Shows progress,
 * auto-completion badges sourced from the webhook auto-trigger
 * service, per-step skip / unskip with confirmation, and a
 * "reset checklist" affordance for re-onboarding.
 */

import { useCallback, useEffect, useState } from "react";

import {
  getOnboardingChecklist,
  resetOnboardingChecklist,
  skipOnboardingStep,
  unskipOnboardingStep,
  type OnboardingChecklist as Checklist,
  type OnboardingStep,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

type ConfirmKind =
  | { kind: "skip"; step: OnboardingStep }
  | { kind: "reset" };

export default function OnboardingChecklist() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [checklist, setChecklist] = useState<Checklist | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [confirm, setConfirm] = useState<ConfirmKind | null>(null);

  const reload = useCallback((tid: string) => {
    setLoading(true);
    getOnboardingChecklist(tid)
      .then(setChecklist)
      .catch((e: unknown) => setError(String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const total = checklist?.steps.length ?? 0;
  const completed = checklist?.steps.filter((s) => s.status === "complete").length ?? 0;
  const skipped = checklist?.steps.filter((s) => s.status === "skipped").length ?? 0;
  const pct = total === 0 ? 0 : Math.round(((completed + skipped) / total) * 100);

  const onSkipClicked = (step: OnboardingStep) => {
    if (step.status === "skipped") {
      runUnskip(step);
    } else {
      setConfirm({ kind: "skip", step });
    }
  };

  const runUnskip = async (step: OnboardingStep) => {
    if (!selectedTenantId) return;
    try {
      await unskipOnboardingStep(selectedTenantId, step.id);
      setInfo(`Step "${step.title}" unskipped.`);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const runSkip = async (step: OnboardingStep) => {
    if (!selectedTenantId) return;
    try {
      await skipOnboardingStep(selectedTenantId, step.id);
      setInfo(`Step "${step.title}" skipped.`);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const runReset = async () => {
    if (!selectedTenantId) return;
    try {
      await resetOnboardingChecklist(selectedTenantId);
      setInfo("Checklist reset.");
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onConfirm = async () => {
    if (!confirm) return;
    if (confirm.kind === "skip") await runSkip(confirm.step);
    if (confirm.kind === "reset") await runReset();
    setConfirm(null);
  };

  const statusIcon = (s: OnboardingStep): string => {
    if (s.status === "complete") return "✓";
    if (s.status === "skipped") return "⤳";
    return "○";
  };

  return (
    <section className="kmail-admin-page">
      <h2>Onboarding checklist</h2>
      <p className="kmail-admin-help">
        Each step has a guided action; some flip to <em>complete</em>{" "}
        automatically the first time KMail sees the matching event
        (e.g. <code>email.received</code>, <code>domain.verified</code>,
        <code>user.created</code> with ≥ 2 users). Optional steps can be
        skipped — that does not block onboarding.
      </p>

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
        {checklist && (
          <button onClick={() => setConfirm({ kind: "reset" })}>Reset checklist</button>
        )}
      </div>

      {error && (
        <p className="kmail-error" role="alert">
          {error} <button onClick={() => setError(null)}>dismiss</button>
        </p>
      )}
      {info && (
        <p className="kmail-info" role="status">
          {info} <button onClick={() => setInfo(null)}>dismiss</button>
        </p>
      )}
      {loading && <p>Loading…</p>}

      {checklist && (
        <>
          <div className="kmail-admin-card" role="region" aria-label="Onboarding progress">
            <strong>Progress: {pct}%</strong>{" "}
            ({completed} complete, {skipped} skipped, {total - completed - skipped} pending)
            <div
              role="progressbar"
              aria-valuenow={pct}
              aria-valuemin={0}
              aria-valuemax={100}
              style={{
                background: "#eee",
                borderRadius: 4,
                marginTop: 8,
                height: 12,
                overflow: "hidden",
              }}
            >
              <div
                style={{
                  width: `${pct}%`,
                  background: "var(--kmail-accent, #4a8)",
                  height: "100%",
                }}
              />
            </div>
          </div>

          <div style={{ display: "grid", gap: "1rem" }}>
            {checklist.steps.map((s) => (
              <div key={s.id} className="kmail-admin-card">
                <h3>
                  <span aria-hidden style={{ marginRight: 8 }}>{statusIcon(s)}</span>
                  {s.title}
                  {s.optional ? <small> (optional)</small> : null}
                  {s.auto_completed && (
                    <small
                      style={{
                        marginLeft: 8,
                        background: "#d1fae5",
                        color: "#065f46",
                        padding: "0 6px",
                        borderRadius: 4,
                        fontSize: 12,
                      }}
                    >
                      completed automatically
                    </small>
                  )}
                </h3>
                <p>{s.description}</p>
                <div style={{ display: "flex", gap: "0.5rem" }}>
                  {s.link && s.status !== "complete" && (
                    <a href={s.link} className="kmail-button">Open</a>
                  )}
                  {s.optional && s.status !== "complete" && (
                    <button onClick={() => onSkipClicked(s)}>
                      {s.status === "skipped" ? "Unskip" : "Skip"}
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>
        </>
      )}

      {confirm && (
        <div role="dialog" aria-modal="true" className="kmail-modal">
          <div className="kmail-modal-body">
            {confirm.kind === "skip" ? (
              <p>
                Skip step <strong>{confirm.step.title}</strong>? You can unskip
                it later if you change your mind.
              </p>
            ) : (
              <p>
                Reset the entire onboarding checklist? Every skip and
                auto-completion mark will be cleared and steps will recompute
                from current tenant state.
              </p>
            )}
            <div style={{ display: "flex", gap: "0.5rem" }}>
              <button onClick={onConfirm}>Confirm</button>
              <button onClick={() => setConfirm(null)}>Cancel</button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
