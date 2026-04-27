/**
 * SieveAdmin lets a tenant admin author Sieve filter rules,
 * validate them, and push the enabled set to Stalwart.
 */
import { useCallback, useEffect, useState } from "react";

import {
  createSieveRule,
  deleteSieveRule,
  deploySieveRules,
  listSieveRules,
  updateSieveRule,
  validateSieveScript,
  type SieveRule,
} from "../../api/admin";
import { useTenantSelection } from "./useTenantSelection";

const blankRule = (): Omit<SieveRule, "id" | "tenant_id" | "created_at" | "updated_at"> => ({
  user_id: null,
  name: "New rule",
  script: "require [\"fileinto\"];\nif header :contains \"subject\" \"[loadtest]\" {\n  fileinto \"loadtest\";\n}\n",
  priority: 100,
  enabled: true,
});

export default function SieveAdmin() {
  const { tenants, selectedTenantId, selectTenant } = useTenantSelection();
  const [rules, setRules] = useState<SieveRule[]>([]);
  const [editing, setEditing] = useState<SieveRule | null>(null);
  const [draft, setDraft] = useState(blankRule());
  const [error, setError] = useState<string | null>(null);
  const [info, setInfo] = useState<string | null>(null);

  const reload = useCallback((tid: string) => {
    listSieveRules(tid)
      .then((r) => setRules(r.rules))
      .catch((e: unknown) => setError(String(e)));
  }, []);

  useEffect(() => {
    if (selectedTenantId) reload(selectedTenantId);
  }, [selectedTenantId, reload]);

  const onValidate = async () => {
    if (!selectedTenantId) return;
    const script = editing?.script ?? draft.script;
    try {
      const r = await validateSieveScript(selectedTenantId, script);
      if (r.valid) setInfo("Script validates clean.");
      else setError(`Validation: ${r.error ?? "invalid"}`);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onSave = async () => {
    if (!selectedTenantId) return;
    try {
      if (editing) {
        await updateSieveRule(selectedTenantId, editing);
      } else {
        await createSieveRule(selectedTenantId, draft);
      }
      setInfo("Saved.");
      setEditing(null);
      setDraft(blankRule());
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDelete = async (id: string) => {
    if (!selectedTenantId) return;
    try {
      await deleteSieveRule(selectedTenantId, id);
      reload(selectedTenantId);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const onDeploy = async () => {
    if (!selectedTenantId) return;
    try {
      await deploySieveRules(selectedTenantId);
      setInfo("Deploy queued.");
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const target = editing
    ? { value: editing, set: setEditing as (r: SieveRule) => void }
    : { value: draft as SieveRule, set: (r: SieveRule) => setDraft(r) };

  return (
    <div className="admin-page">
      <h2>Sieve rules</h2>
      <div className="tenant-picker">
        <label>
          Tenant{" "}
          <select
            value={selectedTenantId ?? ""}
            onChange={(e) => selectTenant(e.target.value)}
          >
            <option value="">— select —</option>
            {(tenants ?? []).map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
        </label>
        <button type="button" onClick={onDeploy}>Deploy enabled rules</button>
      </div>
      {error && <p className="error">{error}</p>}
      {info && <p className="info">{info}</p>}
      <table className="admin-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>Priority</th>
            <th>Enabled</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {rules.map((r) => (
            <tr key={r.id}>
              <td>{r.name}</td>
              <td>{r.priority}</td>
              <td>{r.enabled ? "yes" : "no"}</td>
              <td>
                <button type="button" onClick={() => setEditing(r)}>Edit</button>{" "}
                <button type="button" onClick={() => onDelete(r.id)}>Delete</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <h3>{editing ? "Edit rule" : "New rule"}</h3>
      <label>
        Name{" "}
        <input
          value={target.value.name}
          onChange={(e) =>
            target.set({ ...target.value, name: e.target.value })
          }
        />
      </label>
      <label>
        Priority{" "}
        <input
          type="number"
          value={target.value.priority}
          onChange={(e) =>
            target.set({ ...target.value, priority: Number(e.target.value) })
          }
        />
      </label>
      <label>
        <input
          type="checkbox"
          checked={target.value.enabled}
          onChange={(e) =>
            target.set({ ...target.value, enabled: e.target.checked })
          }
        />
        Enabled
      </label>
      <textarea
        rows={10}
        cols={80}
        value={target.value.script}
        onChange={(e) =>
          target.set({ ...target.value, script: e.target.value })
        }
      />
      <div className="actions">
        <button type="button" onClick={onValidate}>Validate</button>
        <button type="button" onClick={onSave}>Save</button>
        {editing && (
          <button type="button" onClick={() => setEditing(null)}>Cancel</button>
        )}
      </div>
    </div>
  );
}
