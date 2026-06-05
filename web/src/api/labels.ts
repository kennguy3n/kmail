/**
 * Label/tag store.
 *
 * A label is applied to an email as a JMAP keyword (RFC 8621
 * §4.1.1). JMAP keywords are bare tokens with no room for a display
 * name or colour, so this store keeps the presentation metadata
 * (name, colour) client-side and maps each label to a stable
 * keyword token. Applying/removing a label is done through
 * `jmapClient.setKeyword` / `bulkSetKeyword`; this module owns the
 * label registry, not the per-email keyword writes.
 *
 * The keyword token is generated once at creation and never changes
 * (even on rename), so re-labelling existing emails is never
 * required just because a user fixed a typo in a label's name.
 */
import type { Label, LabelDraft } from "../types";
import { newId, readJSON, writeJSON } from "./localStore";

const KEY = "labels";

/**
 * Namespace prefix for label keywords. Keeps user labels clear of
 * JMAP system keywords (which start with `$`, e.g. `$seen`,
 * `$flagged`) and of any other app's keywords on the same account.
 */
const KEYWORD_PREFIX = "kmlabel_";

/** Default palette offered by the label creation UI. */
export const LABEL_COLORS = [
  "#ef4444",
  "#f97316",
  "#eab308",
  "#22c55e",
  "#06b6d4",
  "#3b82f6",
  "#8b5cf6",
  "#ec4899",
  "#64748b",
] as const;

/**
 * Derive a JMAP-safe keyword token from a label name. Lowercases,
 * collapses any run of non-alphanumeric characters to a single
 * underscore, trims leading/trailing underscores, and prefixes the
 * label namespace. A short random suffix guarantees uniqueness so
 * two labels named "Work" / "work" (or two genuinely distinct
 * labels that slug to the same token) never collide.
 */
export function labelKeyword(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
  const base = slug.length > 0 ? slug : "label";
  const suffix = newId().replace(/[^a-z0-9]/g, "").slice(0, 6);
  return `${KEYWORD_PREFIX}${base}_${suffix}`;
}

/** True when `keyword` is one this app manages as a label. */
export function isLabelKeyword(keyword: string): boolean {
  return keyword.startsWith(KEYWORD_PREFIX);
}

/** Return every label, alphabetically by name. */
export function listLabels(): Label[] {
  const list = readJSON<Label[]>(KEY, []);
  return [...list].sort((a, b) =>
    a.name.localeCompare(b.name, undefined, { sensitivity: "base" }),
  );
}

function persist(list: Label[]): void {
  writeJSON(KEY, list);
}

/** Create a new label and return it. */
export function createLabel(draft: LabelDraft): Label {
  const name = draft.name.trim() || "Untitled";
  const label: Label = {
    id: newId(),
    name,
    color: draft.color,
    keyword: labelKeyword(name),
  };
  persist([...listLabels(), label]);
  return label;
}

/**
 * Update a label's display name and/or colour. The keyword is
 * intentionally left untouched so existing emails keep their label.
 */
export function updateLabel(id: string, draft: LabelDraft): Label {
  const list = listLabels();
  const idx = list.findIndex((l) => l.id === id);
  if (idx === -1) {
    throw new Error(`label ${id} not found`);
  }
  const updated: Label = {
    ...list[idx],
    name: draft.name.trim() || list[idx].name,
    color: draft.color,
  };
  const next = [...list];
  next[idx] = updated;
  persist(next);
  return updated;
}

/** Delete a label from the registry. No-op if it doesn't exist. */
export function deleteLabel(id: string): void {
  persist(listLabels().filter((l) => l.id !== id));
}

/** Look up a label by its keyword token, or null. */
export function labelByKeyword(keyword: string): Label | null {
  return listLabels().find((l) => l.keyword === keyword) ?? null;
}

/**
 * Map an email's `keywords` map to the labels it carries. Used by
 * the inbox row to render label chips. Keywords with no registered
 * label (e.g. a label deleted on another device) are skipped.
 */
export function labelsForKeywords(
  keywords: Record<string, boolean>,
): Label[] {
  const labels = listLabels();
  return labels.filter((l) => keywords[l.keyword] === true);
}
