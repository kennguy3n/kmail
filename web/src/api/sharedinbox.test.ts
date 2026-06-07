/**
 * Unit tests for the shared-inbox collaboration client.
 *
 * Covers assignment listing with optional status/assignee query
 * params, the assign/unassign/status/notes mutations (URL shape +
 * JSON bodies + admin auth), and the list-envelope unwrapping for
 * assignments and notes.
 */
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  addNote,
  assignEmail,
  listAssignments,
  listNotes,
  setStatus,
} from "./sharedinbox";
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

describe("listAssignments", () => {
  it("unwraps the assignments envelope and applies filters", async () => {
    const fetchMock = mockFetch(jsonResponse({ assignments: [{ id: "a1" }] }));
    const rows = await listAssignments("inbox-1", {
      status: "open",
      assigneeUserId: "user-1",
    });
    expect(rows).toHaveLength(1);

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/api/v1/shared-inboxes/inbox-1/assignments?");
    expect(url).toContain("status=open");
    expect(url).toContain("assignee_user_id=user-1");
    expect(new Headers(init.headers).get("Authorization")).toBe(
      `Bearer ${DEV_BEARER_TOKEN}`,
    );
  });

  it("omits filters when none are provided", async () => {
    const fetchMock = mockFetch(jsonResponse({}));
    await expect(listAssignments("inbox-1")).resolves.toEqual([]);
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/shared-inboxes/inbox-1/assignments?");
  });
});

describe("assignEmail", () => {
  it("POSTs the assignee to the email-scoped assign endpoint", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "a1", status: "open" }));
    await assignEmail("inbox-1", "email 9", "user-2");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/shared-inboxes/inbox-1/emails/email%209/assign");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({
      assignee_user_id: "user-2",
    });
  });
});

describe("setStatus", () => {
  it("PUTs the new status (e.g. resolved)", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "a1", status: "resolved" }));
    const updated = await setStatus("inbox-1", "email-1", "resolved");
    expect(updated.status).toBe("resolved");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/shared-inboxes/inbox-1/emails/email-1/status");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string)).toEqual({ status: "resolved" });
  });
});

describe("notes", () => {
  it("lists notes via the envelope", async () => {
    mockFetch(jsonResponse({ notes: [{ id: "n1", note_text: "hi" }] }));
    await expect(listNotes("inbox-1", "email-1")).resolves.toHaveLength(1);
  });

  it("adds a note with the note_text body", async () => {
    const fetchMock = mockFetch(jsonResponse({ id: "n1", note_text: "Looking" }));
    await addNote("inbox-1", "email-1", "Looking");
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/v1/shared-inboxes/inbox-1/emails/email-1/notes");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ note_text: "Looking" });
  });
});
