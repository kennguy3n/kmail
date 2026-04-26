/**
 * Typed REST client for the CardDAV contact bridge
 * (`internal/contactbridge`).
 */
import { ADMIN_API_BASE, adminAuthHeaders, requestJSON } from "./admin";
import type { AddressBook, Contact, ContactDraft } from "../types";

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
