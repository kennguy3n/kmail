/**
 * Smoke tests for `web/src/api/jmap.ts`.
 *
 * The JMAP client is the only path the React UI takes to read or
 * write mail and calendar data, so it carries a load-bearing
 * contract with the Go BFF (see docs/JMAP-CONTRACT.md). These
 * tests stub `fetch` and assert on:
 *
 *   1. The wire shape of each request (URL, method, headers, body)
 *      so an accidental edit to the BFF contract surfaces here
 *      before it surfaces in production.
 *   2. The parsing of well-formed responses into typed shapes.
 *   3. Error paths (`JmapMethodError`, missing identities, missing
 *      session capabilities) so callers don't silently swallow
 *      method-level errors that come back inside an HTTP-200
 *      batch response.
 *
 * Each test resets the cached session on the singleton client so
 * tests don't cross-contaminate.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  DEV_BEARER_TOKEN,
  JMAPClient,
  JMAP_SESSION_URL,
  JmapMethodError,
  fetchSession,
} from "./jmap";
import {
  JMAP_CALENDARS_CAPABILITY,
  JMAP_MAIL_CAPABILITY,
  JMAP_SUBMISSION_CAPABILITY,
  type JmapResponse,
  type JmapSession,
} from "../types";

function buildSession(overrides: Partial<JmapSession> = {}): JmapSession {
  return {
    capabilities: { [JMAP_MAIL_CAPABILITY]: {} },
    accounts: {
      "acct-1": {
        name: "alice@example.com",
        isPersonal: true,
        isReadOnly: false,
        accountCapabilities: {},
      },
    },
    primaryAccounts: {
      [JMAP_MAIL_CAPABILITY]: "acct-1",
      [JMAP_CALENDARS_CAPABILITY]: "cal-acct-1",
    },
    username: "alice@example.com",
    apiUrl: "/jmap/api",
    downloadUrl: "/jmap/download/{accountId}/{blobId}/{name}",
    uploadUrl: "/jmap/upload/{accountId}",
    eventSourceUrl: "/jmap/events/",
    state: "00",
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function mockFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn();
  for (const r of responses) {
    fetchMock.mockResolvedValueOnce(r);
  }
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("fetchSession", () => {
  it("GETs the well-known session URL with the dev bearer token", async () => {
    const session = buildSession();
    const fetchMock = mockFetch(jsonResponse(session));

    const got = await fetchSession();

    expect(got).toEqual(session);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(JMAP_SESSION_URL);
    expect(init.credentials).toBe("include");
    const headers = new Headers(init.headers);
    expect(headers.get("Authorization")).toBe(`Bearer ${DEV_BEARER_TOKEN}`);
    expect(headers.get("Accept")).toBe("application/json");
  });

  it("throws when the server returns a non-2xx", async () => {
    mockFetch(new Response("nope", { status: 401, statusText: "Unauthorized" }));
    await expect(fetchSession()).rejects.toThrow(/401/);
  });
});

describe("JMAPClient.getSession", () => {
  it("caches the session document across calls", async () => {
    const session = buildSession();
    const fetchMock = mockFetch(jsonResponse(session), jsonResponse(session));
    const client = new JMAPClient();

    const a = await client.getSession();
    const b = await client.getSession();

    expect(a).toBe(b);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("resetSession() forces a refetch (e.g. after logout)", async () => {
    const session = buildSession();
    const fetchMock = mockFetch(jsonResponse(session), jsonResponse(session));
    const client = new JMAPClient();

    await client.getSession();
    client.resetSession();
    await client.getSession();

    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

describe("JMAPClient.uploadBlob", () => {
  it("posts to the account upload URL and returns the blob id", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse({ blobId: "blob-1", type: "text/plain", size: 3 }),
    );
    const client = new JMAPClient();

    const result = await client.uploadBlob(
      new Blob(["abc"], { type: "text/plain" }),
      "notes.txt",
    );

    expect(result).toEqual({ blobId: "blob-1", type: "text/plain", size: 3 });
    const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(url).toBe("/jmap/upload/acct-1");
    expect(init.method).toBe("POST");
    const headers = new Headers(init.headers);
    expect(headers.get("Content-Disposition")).toBe(
      'attachment; filename="notes.txt"',
    );
  });

  it("escapes quotes/backslashes and strips control chars in the filename header", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse({ blobId: "blob-2" }),
    );
    const client = new JMAPClient();

    // A filename containing a double quote, a backslash and a CRLF —
    // all of which would otherwise break or inject the HTTP header.
    await client.uploadBlob(
      new Blob(["x"]),
      'a"b\\c\r\ninjected: yes.txt',
    );

    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const headers = new Headers(init.headers);
    expect(headers.get("Content-Disposition")).toBe(
      'attachment; filename="a\\"b\\\\cinjected: yes.txt"',
    );
  });
});

describe("JMAPClient.getAccountId", () => {
  it("returns the primary Mail accountId", async () => {
    mockFetch(jsonResponse(buildSession()));
    const client = new JMAPClient();

    expect(await client.getAccountId()).toBe("acct-1");
  });

  it("throws when the session has no primary Mail account", async () => {
    mockFetch(
      jsonResponse(
        buildSession({
          primaryAccounts: { [JMAP_CALENDARS_CAPABILITY]: "cal-acct-1" },
        }),
      ),
    );
    const client = new JMAPClient();

    await expect(client.getAccountId()).rejects.toThrow(
      /no primary Mail account/,
    );
  });
});

describe("JMAPClient.request", () => {
  it("POSTs the batch to session.apiUrl with the Mail+Submission capabilities", async () => {
    const session = buildSession();
    const apiBody: JmapResponse = {
      sessionState: "00",
      methodResponses: [["Core/echo", { ok: true }, "0"]],
    };
    const fetchMock = mockFetch(
      jsonResponse(session),
      jsonResponse(apiBody),
    );
    const client = new JMAPClient();

    const got = await client.request([["Core/echo", {}, "0"]]);

    expect(got).toEqual(apiBody);
    expect(fetchMock).toHaveBeenNthCalledWith(2, session.apiUrl, expect.objectContaining({
      method: "POST",
      credentials: "include",
    }));
    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const headers = new Headers(init.headers);
    expect(headers.get("Content-Type")).toBe("application/json");
    expect(headers.get("Authorization")).toBe(`Bearer ${DEV_BEARER_TOKEN}`);
    const body = JSON.parse(init.body as string) as {
      using: string[];
      methodCalls: unknown[];
    };
    expect(body.using).toEqual([JMAP_MAIL_CAPABILITY, JMAP_SUBMISSION_CAPABILITY]);
    expect(body.methodCalls).toEqual([["Core/echo", {}, "0"]]);
  });
});

describe("JMAPClient.getMailboxes", () => {
  it("issues Mailbox/get with ids:null and returns the list", async () => {
    const session = buildSession();
    const apiBody: JmapResponse = {
      sessionState: "00",
      methodResponses: [
        [
          "Mailbox/get",
          {
            list: [
              {
                id: "mb-1",
                name: "Inbox",
                role: "inbox",
                sortOrder: 0,
                totalEmails: 1,
                unreadEmails: 1,
                totalThreads: 1,
                unreadThreads: 1,
                parentId: null,
                isSubscribed: true,
                myRights: {
                  mayReadItems: true,
                  mayAddItems: true,
                  mayRemoveItems: true,
                  maySetSeen: true,
                  maySetKeywords: true,
                  mayCreateChild: false,
                  mayRename: false,
                  mayDelete: false,
                  maySubmit: true,
                },
              },
            ],
          },
          "0",
        ],
      ],
    };
    const fetchMock = mockFetch(
      jsonResponse(session),
      jsonResponse(apiBody),
    );
    const client = new JMAPClient();

    const mailboxes = await client.getMailboxes();

    expect(mailboxes).toHaveLength(1);
    expect(mailboxes[0]).toMatchObject({ id: "mb-1", role: "inbox" });
    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const body = JSON.parse(init.body as string) as {
      methodCalls: [string, { accountId: string; ids: string[] | null }, string][];
    };
    expect(body.methodCalls[0][0]).toBe("Mailbox/get");
    expect(body.methodCalls[0][1].accountId).toBe("acct-1");
    expect(body.methodCalls[0][1].ids).toBeNull();
  });
});

describe("JMAPClient.searchEmails", () => {
  it("returns [] for a blank query without issuing a request", async () => {
    mockFetch(jsonResponse(buildSession()));
    const client = new JMAPClient();

    const got = await client.searchEmails("   ");

    expect(got).toEqual([]);
  });

  it("scopes the filter to the supplied mailbox id", async () => {
    const session = buildSession();
    const apiBody: JmapResponse = {
      sessionState: "00",
      methodResponses: [
        ["Email/query", { ids: [] }, "0"],
        ["Email/get", { list: [] }, "1"],
      ],
    };
    const fetchMock = mockFetch(
      jsonResponse(session),
      jsonResponse(apiBody),
    );
    const client = new JMAPClient();

    await client.searchEmails("hello", { mailboxId: "mb-1", limit: 10 });

    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const body = JSON.parse(init.body as string) as {
      methodCalls: [string, Record<string, unknown>, string][];
    };
    expect(body.methodCalls[0][0]).toBe("Email/query");
    expect(body.methodCalls[0][1]).toMatchObject({
      accountId: "acct-1",
      filter: {
        operator: "AND",
        conditions: [{ inMailbox: "mb-1" }, { text: "hello" }],
      },
      limit: 10,
    });
  });
});

describe("JMAPClient.resolveOrCreateSnoozedMailbox", () => {
  // Helper to build a Mailbox/get response envelope.
  function mailboxResp(boxes: Array<{ id: string; name: string; role: string | null }>): JmapResponse {
    return {
      sessionState: "00",
      methodResponses: [
        [
          "Mailbox/get",
          {
            accountId: "acct-1",
            state: "00",
            list: boxes.map((b) => ({
              id: b.id,
              name: b.name,
              role: b.role,
              parentId: null,
              totalEmails: 0,
              unreadEmails: 0,
              totalThreads: 0,
              unreadThreads: 0,
              myRights: {
                mayReadItems: true,
                mayAddItems: true,
                mayRemoveItems: true,
                maySetSeen: true,
                maySetKeywords: true,
                mayCreateChild: true,
                mayRename: true,
                mayDelete: true,
                maySubmit: true,
              },
              isSubscribed: true,
            })),
            notFound: [],
          },
          "0",
        ],
      ],
    };
  }

  // Helper to build a Mailbox/set create response.
  function mailboxCreateResp(id: string): JmapResponse {
    return {
      sessionState: "00",
      methodResponses: [
        [
          "Mailbox/set",
          {
            accountId: "acct-1",
            newState: "01",
            created: { mb: { id } },
          },
          "0",
        ],
      ],
    };
  }

  function mailboxCreateRejected(reason: string): JmapResponse {
    return {
      sessionState: "00",
      methodResponses: [
        [
          "Mailbox/set",
          {
            accountId: "acct-1",
            newState: "00",
            notCreated: {
              mb: { type: "invalidProperties", description: reason },
            },
          },
          "0",
        ],
      ],
    };
  }

  it("returns the existing snoozed-by-role mailbox without calling Mailbox/set", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse(
        mailboxResp([
          { id: "mb-snoozed", name: "Whatever", role: "snoozed" },
          { id: "mb-inbox", name: "Inbox", role: "inbox" },
        ]),
      ),
    );
    const client = new JMAPClient();

    const id = await client.resolveOrCreateSnoozedMailbox();

    expect(id).toBe("mb-snoozed");
    // session GET + one Mailbox/get POST — NO Mailbox/set create.
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("returns the existing snoozed-by-name mailbox case-insensitively", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse(
        mailboxResp([
          { id: "mb-inbox", name: "Inbox", role: "inbox" },
          { id: "mb-snz", name: "snoozed", role: null },
        ]),
      ),
    );
    const client = new JMAPClient();

    const id = await client.resolveOrCreateSnoozedMailbox();

    expect(id).toBe("mb-snz");
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("creates a new Snoozed mailbox when no existing match", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse(mailboxResp([{ id: "mb-inbox", name: "Inbox", role: "inbox" }])),
      jsonResponse(mailboxCreateResp("mb-new")),
    );
    const client = new JMAPClient();

    const id = await client.resolveOrCreateSnoozedMailbox();

    expect(id).toBe("mb-new");
    // session + Mailbox/get + Mailbox/set create.
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it("recovers from a concurrent-create race by re-fetching and returning the now-existing id", async () => {
    // Models the cross-view race: between our initial Mailbox/get
    // and our Mailbox/set, another view/tab created the Snoozed
    // mailbox. Our Mailbox/set is rejected (duplicate name), and
    // the helper re-fetches and finds the mailbox now exists.
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      // First Mailbox/get: nothing.
      jsonResponse(mailboxResp([{ id: "mb-inbox", name: "Inbox", role: "inbox" }])),
      // Mailbox/set: server rejects (duplicate / concurrent create won).
      jsonResponse(mailboxCreateRejected("name already in use")),
      // Recovery Mailbox/get: now the mailbox exists.
      jsonResponse(
        mailboxResp([
          { id: "mb-inbox", name: "Inbox", role: "inbox" },
          { id: "mb-race-winner", name: "Snoozed", role: null },
        ]),
      ),
    );
    const client = new JMAPClient();

    const id = await client.resolveOrCreateSnoozedMailbox();

    expect(id).toBe("mb-race-winner");
    // session + Mailbox/get + failed Mailbox/set + recovery Mailbox/get.
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it("surfaces the create error when the mailbox still isn't found on recovery re-fetch", async () => {
    // Models a genuine non-recoverable failure: create rejected
    // AND the re-fetch still doesn't show the mailbox (so it
    // wasn't a concurrent-create race; the create just failed
    // for a real reason).
    mockFetch(
      jsonResponse(buildSession()),
      jsonResponse(mailboxResp([{ id: "mb-inbox", name: "Inbox", role: "inbox" }])),
      jsonResponse(mailboxCreateRejected("quota exceeded")),
      jsonResponse(mailboxResp([{ id: "mb-inbox", name: "Inbox", role: "inbox" }])),
    );
    const client = new JMAPClient();

    await expect(client.resolveOrCreateSnoozedMailbox()).rejects.toThrow(
      /quota exceeded/,
    );
  });
});

describe("JMAPClient.bulkMove", () => {
  function setResp(ids: string[]): JmapResponse {
    const updated: Record<string, null> = {};
    for (const id of ids) updated[id] = null;
    return {
      sessionState: "00",
      methodResponses: [["Email/set", { updated }, "0"]],
    };
  }

  it("removes the source mailbox when from and to differ", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse(setResp(["e1"])),
    );
    const client = new JMAPClient();

    await client.bulkMove(["e1"], "mb-from", "mb-to");

    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const body = JSON.parse(init.body as string) as {
      methodCalls: [string, { update: Record<string, Record<string, unknown>> }, string][];
    };
    expect(body.methodCalls[0][0]).toBe("Email/set");
    expect(body.methodCalls[0][1].update.e1).toEqual({
      "mailboxIds/mb-to": true,
      "mailboxIds/mb-from": null,
    });
  });

  it("emits an add-only patch (no remove) when from === to", async () => {
    const fetchMock = mockFetch(
      jsonResponse(buildSession()),
      jsonResponse(setResp(["e1"])),
    );
    const client = new JMAPClient();

    await client.bulkMove(["e1"], "mb-x", "mb-x");

    const [, init] = fetchMock.mock.calls[1] as [string, RequestInit];
    const body = JSON.parse(init.body as string) as {
      methodCalls: [string, { update: Record<string, Record<string, unknown>> }, string][];
    };
    // Must NOT contain a `mailboxIds/mb-x: null` removal — that would
    // leave the email in zero mailboxes (rejected by RFC 8621).
    expect(body.methodCalls[0][1].update.e1).toEqual({
      "mailboxIds/mb-x": true,
    });
  });
});

describe("JmapMethodError", () => {
  it("exposes the method/callId/result triple from an error invocation", () => {
    const err = new JmapMethodError([
      "Email/get",
      { type: "invalidArguments", description: "bad ids" },
      "0",
    ]);

    expect(err).toBeInstanceOf(Error);
    expect(err.method).toBe("Email/get");
    expect(err.callId).toBe("0");
    expect(err.result).toMatchObject({ type: "invalidArguments" });
    expect(err.message).toContain("invalidArguments");
    expect(err.message).toContain("bad ids");
  });
});
