/**
 * Email template store + variable expansion.
 *
 * Templates are persisted client-side (localStorage) in the first
 * phase, mirroring the signature store. The CRUD surface is storage
 * agnostic so a future per-user / per-tenant backend can replace
 * persistence without touching callers.
 *
 * Templates support `{{variable}}` placeholders. `renderTemplate`
 * expands them against a caller-supplied value map, after seeding
 * the built-ins (`sender_name`, `company`, `date`). Unknown
 * placeholders are left intact so a typo'd variable is visible in
 * the composed message rather than silently blanked.
 */
import type { EmailTemplate, EmailTemplateDraft } from "../types";
import { newId, readJSON, writeJSON } from "./localStore";

const KEY = "templates";

function nowISO(): string {
  return new Date().toISOString();
}

/** Return every saved template, alphabetically by name. */
export function listTemplates(): EmailTemplate[] {
  const list = readJSON<EmailTemplate[]>(KEY, []);
  return [...list].sort((a, b) =>
    a.name.localeCompare(b.name, undefined, { sensitivity: "base" }),
  );
}

function persist(list: EmailTemplate[]): void {
  writeJSON(KEY, list);
}

/** Create a new template and return it. */
export function createTemplate(draft: EmailTemplateDraft): EmailTemplate {
  const tpl: EmailTemplate = {
    id: newId(),
    name: draft.name.trim() || "Untitled template",
    subject: draft.subject,
    body: draft.body,
    scope: draft.scope,
    createdAt: nowISO(),
    updatedAt: nowISO(),
  };
  persist([...listTemplates(), tpl]);
  return tpl;
}

/** Update an existing template. Throws if the id is unknown. */
export function updateTemplate(
  id: string,
  draft: EmailTemplateDraft,
): EmailTemplate {
  const list = listTemplates();
  const idx = list.findIndex((t) => t.id === id);
  if (idx === -1) {
    throw new Error(`template ${id} not found`);
  }
  const updated: EmailTemplate = {
    ...list[idx],
    name: draft.name.trim() || "Untitled template",
    subject: draft.subject,
    body: draft.body,
    scope: draft.scope,
    updatedAt: nowISO(),
  };
  const next = [...list];
  next[idx] = updated;
  persist(next);
  return updated;
}

/** Delete a template by id. No-op if it doesn't exist. */
export function deleteTemplate(id: string): void {
  persist(listTemplates().filter((t) => t.id !== id));
}

/**
 * The set of built-in variables every template render seeds. The
 * caller's explicit values win over these defaults so a template
 * picker can override `sender_name` with the signed-in identity.
 */
export function builtinVariables(
  overrides: Record<string, string> = {},
): Record<string, string> {
  return {
    date: new Date().toLocaleDateString(),
    sender_name: "",
    company: "",
    ...overrides,
  };
}

/**
 * Expand `{{variable}}` placeholders in `input` against `values`.
 * Whitespace inside the braces is tolerated (`{{ name }}`).
 * Placeholders with no matching value are returned unchanged so the
 * gap is visible to the author rather than silently dropped.
 */
export function renderTemplate(
  input: string,
  values: Record<string, string>,
): string {
  return input.replace(/\{\{\s*([\w.]+)\s*\}\}/g, (match, name: string) => {
    const value = values[name];
    return value === undefined ? match : value;
  });
}

/**
 * List the distinct variable names referenced by a template body +
 * subject, in first-seen order. Used by the picker to prompt for
 * values the built-ins don't cover.
 */
export function extractVariables(...sources: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const re = /\{\{\s*([\w.]+)\s*\}\}/g;
  for (const src of sources) {
    let m: RegExpExecArray | null;
    while ((m = re.exec(src)) !== null) {
      if (!seen.has(m[1])) {
        seen.add(m[1]);
        out.push(m[1]);
      }
    }
  }
  return out;
}
