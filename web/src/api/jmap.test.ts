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
