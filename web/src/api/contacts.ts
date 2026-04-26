/**
 * Typed REST client for the CardDAV contact bridge
 * (`internal/contactbridge`).
 */
import { ADMIN_API_BASE, adminAuthHeaders, requestJSON } from "./admin";
import type { AddressBook, Contact, ContactDraft, GalEntry } from "../types";

export async function listAddressBooks(
  tenantId: string,
  accountId: string,
): Promise<AddressBook[]> {
  return requestJSON<AddressBook[]>(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/addressbooks`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function listContacts(
  tenantId: string,
  accountId: string,
  addressBookId: string,
): Promise<Contact[]> {
  return requestJSON<Contact[]>(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function getContact(
  tenantId: string,
  accountId: string,
  addressBookId: string,
  uid: string,
): Promise<Contact> {
  return requestJSON<Contact>(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}/${encodeURIComponent(uid)}`,
    { headers: adminAuthHeaders(tenantId, { Accept: "application/json" }) },
  );
}

export async function createContact(
  tenantId: string,
  accountId: string,
  addressBookId: string,
  draft: ContactDraft,
): Promise<{ uid: string }> {
  return requestJSON<{ uid: string }>(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        "Content-Type": "application/json",
        Accept: "application/json",
      }),
      body: JSON.stringify(draft),
    },
  );
}

export async function updateContact(
  tenantId: string,
  accountId: string,
  addressBookId: string,
  uid: string,
  draft: ContactDraft,
): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}/${encodeURIComponent(uid)}`,
    {
      method: "PUT",
      headers: adminAuthHeaders(tenantId, { "Content-Type": "application/json" }),
      body: JSON.stringify(draft),
    },
  );
}

export async function deleteContact(
  tenantId: string,
  accountId: string,
  addressBookId: string,
  uid: string,
): Promise<void> {
  await fetch(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}/${encodeURIComponent(uid)}`,
    { method: "DELETE", headers: adminAuthHeaders(tenantId) },
  );
}

export async function importVCard(
  tenantId: string,
  accountId: string,
  addressBookId: string,
  vcfText: string,
): Promise<{ created: number; failed: number }> {
  return requestJSON<{ created: number; failed: number }>(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}/import`,
    {
      method: "POST",
      headers: adminAuthHeaders(tenantId, {
        "Content-Type": "text/vcard",
        Accept: "application/json",
      }),
      body: vcfText,
    },
  );
}

export async function exportVCard(
  tenantId: string,
  accountId: string,
  addressBookId: string,
): Promise<string> {
  const resp = await fetch(
    `${ADMIN_API_BASE}/contacts/${encodeURIComponent(accountId)}/${encodeURIComponent(addressBookId)}/export`,
    { headers: adminAuthHeaders(tenantId, { Accept: "text/vcard" }) },
  );
  if (!resp.ok) throw new Error(`export failed: ${resp.status}`);
  return resp.text();
}

export async function getGlobalAddressList(
  tenantId: string,
): Promise<GalEntry[]> {
  return requestJSON<GalEntry[]>(`${ADMIN_API_BASE}/contacts/gal`, {
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

export async function searchGlobalAddressList(
  tenantId: string,
  query: string,
): Promise<GalEntry[]> {
  const url = `${ADMIN_API_BASE}/contacts/gal/search?q=${encodeURIComponent(query)}`;
  return requestJSON<GalEntry[]>(url, {
    headers: adminAuthHeaders(tenantId, { Accept: "application/json" }),
  });
}

export async function syncGlobalAddressList(
  tenantId: string,
  accounts: string[],
): Promise<{ upserted: number }> {
  return requestJSON<{ upserted: number }>(`${ADMIN_API_BASE}/contacts/gal/sync`, {
    method: "POST",
    headers: adminAuthHeaders(tenantId, {
      "Content-Type": "application/json",
      Accept: "application/json",
    }),
    body: JSON.stringify({ accounts }),
  });
}
