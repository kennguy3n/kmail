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
  DEV_BEARER_TOKEN: "kmail-dev",
  jmapClient: {
    getMailboxes: () => getMailboxes(),
    getIdentities: () => getIdentities(),
    sendEmail: (
      draft: unknown,
      savedId?: string | null,
      options?: unknown,
    ) => sendEmail(draft, savedId, options),
    saveDraft: (draft: unknown, savedId?: string | null) =>
      saveDraft(draft, savedId),
  },
  uploadLargeAttachment: vi.fn(),
}));

vi.mock("../../api/confidentialSend", () => ({
  createSecureMessage: vi.fn(),
}));

const cancelPendingSend = vi.fn();
vi.mock("../../api/undoSend", () => ({
  cancelPendingSend: (id: string) => cancelPendingSend(id),
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
    // BFF without the Undo-Send hook returns no pending-send id
    // — simulate the immediate-dispatch happy path.
    sendEmail.mockResolvedValueOnce({
      emailId: "e-sent-1",
      pendingSendId: null,
      undoDeadline: null,
    });

    const user = userEvent.setup();
    renderCompose();

    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.type(screen.getByLabelText(/^subject/i), "Welcome");
    await user.click(screen.getByRole("button", { name: /^send$/i }));

    await waitFor(() => expect(sendEmail).toHaveBeenCalledTimes(1));
    const [draft, savedId, options] = sendEmail.mock.calls[0];
    expect(savedId).toBeNull();
    expect(options).toMatchObject({ undoSend: true });
    expect(draft).toMatchObject({
      mailboxIds: { "mb-drafts": true },
      to: [{ name: null, email: "alice@example.com" }],
      subject: "Welcome",
      from: [{ name: "Self", email: "self@kmail.test" }],
      privacyMode: "standard",
      identityId: "id-1",
    });
  });

  it("renders the Undo banner and clears it after the deadline elapses", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    sendEmail.mockResolvedValueOnce({
      emailId: "e-sent-undo",
      pendingSendId: "ps-1",
      // 800 ms in the future — short enough to keep the test fast
      // but long enough that the banner is visible before tick 0.
      undoDeadline: new Date(Date.now() + 800),
    });

    const user = userEvent.setup();
    renderCompose();
    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.click(screen.getByRole("button", { name: /^send$/i }));

    const undoBtn = await screen.findByTestId("undo-send-cancel");
    expect(undoBtn).toBeEnabled();
    // Deadline expires within the timeout: banner disappears,
    // success toast appears, and the route navigates to /mail.
    await waitFor(
      () => expect(screen.queryByTestId("undo-send-cancel")).toBeNull(),
      { timeout: 3000 },
    );
    await screen.findByText(/back to mail/i);
  });

  it("cancels the pending send when the Undo button is clicked", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    sendEmail.mockResolvedValueOnce({
      emailId: "e-sent-cancel",
      pendingSendId: "ps-2",
      undoDeadline: new Date(Date.now() + 60_000),
    });
    cancelPendingSend.mockResolvedValueOnce({ cancelled: true });

    const user = userEvent.setup();
    renderCompose();
    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.click(screen.getByRole("button", { name: /^send$/i }));

    const undoBtn = await screen.findByTestId("undo-send-cancel");
    await user.click(undoBtn);

    await waitFor(() =>
      expect(cancelPendingSend).toHaveBeenCalledWith("ps-2"),
    );
    expect(
      await screen.findByText(/send cancelled\. edit the message/i),
    ).toBeInTheDocument();
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
