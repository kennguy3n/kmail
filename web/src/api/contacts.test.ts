/**
 * Unit tests for the CardDAV contact-bridge client.
 *
 * Verifies the nested path construction (account / address-book /
 * uid segments are URL-encoded), the tenant-scoped admin auth
 * headers, the vCard import/export content negotiation, and the GAL
 * search query encoding.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createContact,
  exportVCard,
  importVCard,
  listContacts,
  searchGlobalAddressList,
} from "./contacts";
import { DEV_BEARER_TOKEN } from "./jmap";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function mockFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn();
  for (const r of responses) fetchMock.mockResolvedValueOnce(r);
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("listContacts", () => {
  it("builds the account/address-book path with tenant auth", async () => {
    const fetchMock = mockFetch(jsonResponse([{ uid: "c1" }]));
    await listContacts("tenant-1", "acct-1", "ab 1");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/contacts/acct-1/ab%201");
    const headers = new Headers(init.headers);
    expect(headers.get("Authorization")).toBe(`Bearer ${DEV_BEARER_TOKEN}`);
    expect(headers.get("X-KMail-Dev-Tenant-Id")).toBe("tenant-1");
  });
});

describe("createContact", () => {
  it("POSTs the draft as JSON and returns the new uid", async () => {
    const fetchMock = mockFetch(jsonResponse({ uid: "c-new" }));
    const draft = { fullName: "Ada Lovelace", emails: ["ada@x.com"] };
    const result = await createContact(
      "tenant-1",
      "acct-1",
      "ab-1",
      draft as never,
    );
    expect(result).toEqual({ uid: "c-new" });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/contacts/acct-1/ab-1");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toMatchObject({
      fullName: "Ada Lovelace",
    });
  });
});

describe("importVCard", () => {
  it("POSTs raw vCard text with a text/vcard content type", async () => {
    const fetchMock = mockFetch(jsonResponse({ created: 2, failed: 0 }));
    const vcf = "BEGIN:VCARD\nVERSION:3.0\nFN:Ada\nEND:VCARD";
    const result = await importVCard("tenant-1", "acct-1", "ab-1", vcf);
    expect(result).toEqual({ created: 2, failed: 0 });

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/contacts/acct-1/ab-1/import");
    expect(new Headers(init.headers).get("Content-Type")).toBe("text/vcard");
    expect(init.body).toBe(vcf);
  });
});

describe("exportVCard", () => {
  it("returns the raw vCard body on success", async () => {
    const vcf = "BEGIN:VCARD\nEND:VCARD";
    mockFetch(new Response(vcf, { status: 200 }));
    await expect(exportVCard("t1", "acct-1", "ab-1")).resolves.toBe(vcf);
  });

  it("throws on a non-ok export", async () => {
    mockFetch(new Response("", { status: 500 }));
    await expect(exportVCard("t1", "acct-1", "ab-1")).rejects.toThrow(
      /export failed: 500/,
    );
  });
});

describe("searchGlobalAddressList", () => {
  it("encodes the query string", async () => {
    const fetchMock = mockFetch(jsonResponse([]));
    await searchGlobalAddressList("t1", "ada lovelace");
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/contacts/gal/search?q=ada%20lovelace");
  });
});
