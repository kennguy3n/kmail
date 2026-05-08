/**
 * Component test for Inbox.tsx.
 *
 * Verifies the page-level contract that an integration test of the
 * BFF would not catch:
 *
 *   - On mount, renders mailboxes from `jmapClient.getMailboxes()`
 *     in the sidebar.
 *   - Auto-selects the `inbox` role mailbox and fetches its
 *     messages via `jmapClient.getEmails()`.
 *   - Surfaces fetch errors to the user instead of leaving a
 *     permanent loading state.
 *
 * The full Inbox covers a lot of behaviour (search modes, mark
 * read, mark spam, move to trash); this file pins down the
 * load-bearing first paint so a regression in mailbox/email
 * wiring fails CI immediately. Deeper interaction tests can be
 * layered on later without touching the smoke surface.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import Inbox from "./Inbox";
import type { Email, Mailbox } from "../../types";

const mailboxes: Mailbox[] = [
  {
    id: "mb-inbox",
    name: "Inbox",
    parentId: null,
    role: "inbox",
    sortOrder: 0,
    totalEmails: 2,
    unreadEmails: 1,
    totalThreads: 2,
    unreadThreads: 1,
    isSubscribed: true,
    myRights: rights(),
  },
  {
    id: "mb-sent",
    name: "Sent",
    parentId: null,
    role: "sent",
    sortOrder: 1,
    totalEmails: 5,
    unreadEmails: 0,
    totalThreads: 5,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
];

const emails: Email[] = [
  {
    id: "e-1",
    blobId: "blob-1",
    threadId: "t-1",
    mailboxIds: { "mb-inbox": true },
    keywords: { $seen: false },
    size: 1024,
    receivedAt: new Date().toISOString(),
    from: [{ name: "Alice", email: "alice@example.com" }],
    to: [{ name: null, email: "self@kmail.test" }],
    cc: null,
    bcc: null,
    replyTo: null,
    subject: "Welcome to KMail",
    sentAt: new Date().toISOString(),
    preview: "Thanks for trying KMail.",
  },
];

const getMailboxes = vi.fn();
const getEmails = vi.fn();
const searchEmails = vi.fn();
const markRead = vi.fn();
const markAsSpam = vi.fn();
const deleteEmail = vi.fn();

vi.mock("../../api/jmap", () => ({
  jmapClient: {
    getMailboxes: () => getMailboxes(),
    getEmails: (mailboxId: string, opts?: unknown) =>
      getEmails(mailboxId, opts),
    searchEmails: (q: string, opts?: unknown) => searchEmails(q, opts),
    markRead: (emailId: string, read: boolean) => markRead(emailId, read),
    markAsSpam: (emailId: string, junkId: string, srcId: string) =>
      markAsSpam(emailId, junkId, srcId),
    deleteEmail: (emailId: string) => deleteEmail(emailId),
  },
}));

function renderInbox() {
  return render(
    <MemoryRouter initialEntries={["/mail"]}>
      <Routes>
        <Route path="/mail" element={<Inbox />} />
        <Route path="/mail/:mailboxId" element={<Inbox />} />
        <Route path="/mail/message/:emailId" element={<div>message</div>} />
        <Route path="/mail/compose" element={<div>compose</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("<Inbox />", () => {
  it("renders sidebar mailboxes from getMailboxes()", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getEmails.mockResolvedValueOnce(emails);

    renderInbox();

    expect(await screen.findByText("Inbox")).toBeInTheDocument();
    expect(screen.getByText("Sent")).toBeInTheDocument();
  });

  it("auto-selects the inbox-role mailbox and fetches its emails", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getEmails.mockResolvedValueOnce(emails);

    renderInbox();

    expect(await screen.findByText("Welcome to KMail")).toBeInTheDocument();
    await waitFor(() => {
      expect(getEmails).toHaveBeenCalledWith("mb-inbox", { limit: 50 });
    });
  });

  it("surfaces a fetch error message instead of staying in loading state", async () => {
    getMailboxes.mockRejectedValueOnce(new Error("boom"));
    // getEmails never gets called because mailbox load failed first.

    renderInbox();

    expect(await screen.findByText(/boom/i)).toBeInTheDocument();
  });
});

function rights() {
  return {
    mayReadItems: true,
    mayAddItems: true,
    mayRemoveItems: true,
    maySetSeen: true,
    maySetKeywords: true,
    mayCreateChild: true,
    mayRename: true,
    mayDelete: true,
    maySubmit: true,
  };
}
