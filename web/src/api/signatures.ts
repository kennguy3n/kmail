/**
 * Email signature store.
 *
 * Per the WS2 plan, signatures are persisted client-side
 * (localStorage) in the first phase. The store is deliberately a
 * thin CRUD layer over a normalized {@link Signature} list so a
 * future `internal/jmap/signature.go` (user-preferences row) can
 * replace the persistence without changing any caller — Compose
 * and the SignatureEditor only depend on the functions exported
 * here, never on the storage mechanism.
 *
 * Invariant: at most one signature is `isDefault` per identity
 * scope (a given `identityEmail`, or the `null` "any identity"
 * scope). The store enforces this on every save so the auto-append
 * resolver never has to disambiguate two defaults.
 */
import type { Signature, SignatureDraft } from "../types";
import { newId, readJSON, writeJSON } from "./localStore";

const KEY = "signatures";

function nowISO(): string {
  return new Date().toISOString();
}

/** Return every saved signature, newest-updated first. */
export function listSignatures(): Signature[] {
  const list = readJSON<Signature[]>(KEY, []);
  return [...list].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
}

function persist(list: Signature[]): void {
  writeJSON(KEY, list);
}

/**
 * Clear `isDefault` on every other signature that shares
 * `identityEmail` with `keepId`, so a newly-defaulted signature is
 * the only default in its scope. Two signatures scoped to
 * different identities can both be default.
 */
function dedupeDefault(
  list: Signature[],
  keepId: string,
  identityEmail: string | null,
): Signature[] {
  return list.map((s) =>
    s.id !== keepId && s.identityEmail === identityEmail && s.isDefault
      ? { ...s, isDefault: false, updatedAt: nowISO() }
      : s,
  );
}

/** Create a new signature and return it. */
export function createSignature(draft: SignatureDraft): Signature {
  const sig: Signature = {
    id: newId(),
    name: draft.name.trim() || "Untitled signature",
    html: draft.html,
    identityEmail: draft.identityEmail,
    isDefault: draft.isDefault,
    createdAt: nowISO(),
    updatedAt: nowISO(),
  };
  let list = [...listSignatures(), sig];
  if (sig.isDefault) list = dedupeDefault(list, sig.id, sig.identityEmail);
  persist(list);
  return sig;
}

/** Update an existing signature. Throws if the id is unknown. */
export function updateSignature(
  id: string,
  draft: SignatureDraft,
): Signature {
  const list = listSignatures();
  const idx = list.findIndex((s) => s.id === id);
  if (idx === -1) {
    throw new Error(`signature ${id} not found`);
  }
  const updated: Signature = {
    ...list[idx],
    name: draft.name.trim() || "Untitled signature",
    html: draft.html,
    identityEmail: draft.identityEmail,
    isDefault: draft.isDefault,
    updatedAt: nowISO(),
  };
  let next = [...list];
  next[idx] = updated;
  if (updated.isDefault) {
    next = dedupeDefault(next, updated.id, updated.identityEmail);
  }
  persist(next);
  return updated;
}

/** Delete a signature by id. No-op if it doesn't exist. */
export function deleteSignature(id: string): void {
  persist(listSignatures().filter((s) => s.id !== id));
}

/**
 * Resolve the signature to auto-append when sending under
 * `identityEmail`. Prefers the default scoped to that exact
 * identity, then the default scoped to "any identity" (`null`), and
 * returns `null` when neither exists. Case-insensitive on the
 * identity email so `From` casing differences don't miss a match.
 */
export function defaultSignatureFor(
  identityEmail: string | null,
): Signature | null {
  const list = listSignatures();
  const target = identityEmail?.trim().toLowerCase() ?? null;
  if (target) {
    const scoped = list.find(
      (s) =>
        s.isDefault &&
        s.identityEmail !== null &&
        s.identityEmail.trim().toLowerCase() === target,
    );
    if (scoped) return scoped;
  }
  return list.find((s) => s.isDefault && s.identityEmail === null) ?? null;
}
