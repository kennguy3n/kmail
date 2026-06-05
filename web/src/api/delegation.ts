/**
 * Delegate-access / send-as grant store.
 *
 * Delegation is a product concept layered on top of JMAP: a user
 * grants another principal read or read-write access to their
 * mailbox and/or the right to send under their identity. Stalwart's
 * ACL model is coarser than the product UX, so until a dedicated
 * backend lands these grants are persisted client-side (mirroring
 * the signature/template stores) and the CRUD surface here is the
 * single seam a future `internal/delegation` service would replace.
 *
 * The "send-as" side of a grant is the one with a live JMAP
 * counterpart today: the From dropdown in Compose is populated from
 * `Identity/get` (the identities Stalwart already lets the user
 * send under). This store records the *intended* grants so an admin
 * can manage them and so the UI can show pending/explicit send-as
 * relationships alongside the server-resolved identities.
 */
import type { DelegationGrant, DelegationGrantDraft } from "../types";
import { newId, readJSON, writeJSON } from "./localStore";

const KEY = "delegation.grants";

function nowISO(): string {
  return new Date().toISOString();
}

/** Return every delegation grant, newest first. */
export function listGrants(): DelegationGrant[] {
  const list = readJSON<DelegationGrant[]>(KEY, []);
  return [...list].sort((a, b) => b.createdAt.localeCompare(a.createdAt));
}

function persist(list: DelegationGrant[]): void {
  writeJSON(KEY, list);
}

function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}

/**
 * Create a grant. Rejects self-delegation and duplicate
 * owner→delegate pairs (the latter is updated in place by
 * {@link updateGrant} instead), so the list stays a clean set of
 * distinct relationships.
 */
export function createGrant(draft: DelegationGrantDraft): DelegationGrant {
  const owner = normalizeEmail(draft.ownerEmail);
  const delegate = normalizeEmail(draft.delegateEmail);
  if (!owner || !delegate) {
    throw new Error("Both owner and delegate email are required");
  }
  if (owner === delegate) {
    throw new Error("A user cannot delegate access to themselves");
  }
  const existing = listGrants().find(
    (g) =>
      normalizeEmail(g.ownerEmail) === owner &&
      normalizeEmail(g.delegateEmail) === delegate,
  );
  if (existing) {
    throw new Error(
      `${draft.delegateEmail} already has a grant on ${draft.ownerEmail}; edit it instead`,
    );
  }
  const grant: DelegationGrant = {
    id: newId(),
    ownerEmail: draft.ownerEmail.trim(),
    delegateEmail: draft.delegateEmail.trim(),
    access: draft.access,
    sendAs: draft.sendAs,
    createdAt: nowISO(),
  };
  persist([grant, ...listGrants()]);
  return grant;
}

/** Update a grant's access level / send-as flag. */
export function updateGrant(
  id: string,
  patch: Pick<DelegationGrantDraft, "access" | "sendAs">,
): DelegationGrant {
  const list = listGrants();
  const idx = list.findIndex((g) => g.id === id);
  if (idx === -1) {
    throw new Error(`grant ${id} not found`);
  }
  const updated: DelegationGrant = {
    ...list[idx],
    access: patch.access,
    sendAs: patch.sendAs,
  };
  const next = [...list];
  next[idx] = updated;
  persist(next);
  return updated;
}

/** Revoke (delete) a grant by id. */
export function deleteGrant(id: string): void {
  persist(listGrants().filter((g) => g.id !== id));
}

/**
 * The set of owner emails that have granted `delegateEmail` the
 * send-as right. Compose uses this to surface delegated identities
 * the user may send on behalf of, in addition to their own.
 */
export function sendAsOwnersFor(delegateEmail: string): string[] {
  const target = normalizeEmail(delegateEmail);
  return listGrants()
    .filter((g) => g.sendAs && normalizeEmail(g.delegateEmail) === target)
    .map((g) => g.ownerEmail);
}
