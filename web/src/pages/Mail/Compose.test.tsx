/**
 * Component test for Compose.tsx.
 *
 * Pins down the load-bearing Compose flow without exercising the
 * full reply / forward / Confidential Send fan-out:
 *
 *   - On mount, the component fetches mailboxes and identities in
 *     parallel via `jmapClient.getMailboxes()` /
 *     `jmapClient.getIdentities()`.
 *   - The Send button is disabled until the user types a To
 *     address — the BFF would reject an empty submission anyway,
 *     but failing in the UI is faster and avoids wasting a JMAP
 *     round-trip.
 *   - Clicking Send issues exactly one `jmapClient.sendEmail()`
 *     call carrying the typed To, subject, and body, with the
 *     resolved Identity id and a `mailboxIds` map that targets
 *     the Drafts mailbox.
 *
 * The `useTenantSelection` hook is stubbed out because the tenant
 * selection is only used by Confidential Send — Standard mode
 * (the path tested here) does not depend on it.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import Compose from "./Compose";
import type { Identity, Mailbox } from "../../types";

const mailboxes: Mailbox[] = [
  {
    id: "mb-drafts",
    name: "Drafts",
    parentId: null,
    role: "drafts",
    sortOrder: 0,
    totalEmails: 0,
    unreadEmails: 0,
    totalThreads: 0,
    unreadThreads: 0,
    isSubscribed: true,
    myRights: rights(),
  },
];

const identities: Identity[] = [
  {
    id: "id-1",
    name: "Self",
    email: "self@kmail.test",
    replyTo: null,
    bcc: null,
    textSignature: null,
    htmlSignature: null,
    mayDelete: false,
  },
];

const getMailboxes = vi.fn();
const getIdentities = vi.fn();
const sendEmail = vi.fn();
const saveDraft = vi.fn();

vi.mock("../../api/jmap", () => ({
  ATTACHMENT_LINK_THRESHOLD_BYTES: 10 * 1024 * 1024,
  jmapClient: {
    getMailboxes: () => getMailboxes(),
    getIdentities: () => getIdentities(),
    sendEmail: (draft: unknown, savedId?: string | null) =>
      sendEmail(draft, savedId),
    saveDraft: (draft: unknown, savedId?: string | null) =>
      saveDraft(draft, savedId),
  },
  uploadLargeAttachment: vi.fn(),
}));

vi.mock("../../api/confidentialSend", () => ({
  createSecureMessage: vi.fn(),
}));

vi.mock("../Admin/useTenantSelection", () => ({
  useTenantSelection: () => ({ selectedTenantId: null, setSelectedTenantId: vi.fn() }),
}));

function renderCompose() {
  return render(
    <MemoryRouter initialEntries={["/mail/compose"]}>
      <Routes>
        <Route path="/mail/compose" element={<Compose />} />
        <Route path="/mail" element={<div>back to mail</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("<Compose />", () => {
  it("loads mailboxes and identities on mount and renders the form", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);

    renderCompose();

    await waitFor(() => {
      expect(getMailboxes).toHaveBeenCalledTimes(1);
      expect(getIdentities).toHaveBeenCalledTimes(1);
    });
    expect(
      screen.getByRole("heading", { name: /new message/i }),
    ).toBeInTheDocument();
  });

  it("disables the Send button until a To address is typed", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);

    const user = userEvent.setup();
    renderCompose();

    const sendBtn = await screen.findByRole("button", { name: /^send$/i });
    expect(sendBtn).toBeDisabled();

    const toField = screen.getByLabelText(/^to/i);
    await user.type(toField, "alice@example.com");

    await waitFor(() => expect(sendBtn).toBeEnabled());
  });

  it("invokes jmapClient.sendEmail() with the typed recipient and subject", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    sendEmail.mockResolvedValueOnce("e-sent-1");

    const user = userEvent.setup();
    renderCompose();

    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.type(screen.getByLabelText(/^subject/i), "Welcome");
    await user.click(screen.getByRole("button", { name: /^send$/i }));

    await waitFor(() => expect(sendEmail).toHaveBeenCalledTimes(1));
    const [draft, savedId] = sendEmail.mock.calls[0];
    expect(savedId).toBeNull();
    expect(draft).toMatchObject({
      mailboxIds: { "mb-drafts": true },
      to: [{ name: null, email: "alice@example.com" }],
      subject: "Welcome",
      from: [{ name: "Self", email: "self@kmail.test" }],
      privacyMode: "standard",
      identityId: "id-1",
    });
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
