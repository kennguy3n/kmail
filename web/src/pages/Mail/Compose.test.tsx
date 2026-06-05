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
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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

const recordSend = vi.fn();
const getCoRecipients = vi.fn();
vi.mock("../../api/smart", () => ({
  getFrequentContacts: () => Promise.resolve({ contacts: [] }),
  getCoRecipients: (anchor: string, exclude: string[]) => {
    getCoRecipients(anchor, exclude);
    return Promise.resolve({ anchor, suggestions: [] });
  },
  recordSend: (recipients: string[]) => {
    recordSend(recipients);
    return Promise.resolve();
  },
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
      scheduledSendId: null,
      scheduledSendAt: null,
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

  it("records only the visible recipients (To + Cc, never Bcc) after a successful send", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    sendEmail.mockResolvedValueOnce({
      emailId: "e-sent-rec",
      pendingSendId: null,
      undoDeadline: null,
      scheduledSendId: null,
      scheduledSendAt: null,
    });
    recordSend.mockClear();

    const user = userEvent.setup();
    renderCompose();

    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.type(screen.getByLabelText(/^cc/i), "carol@example.com");
    await user.type(screen.getByLabelText(/^bcc/i), "secret@example.com");
    await user.click(screen.getByRole("button", { name: /^send$/i }));

    await waitFor(() => expect(recordSend).toHaveBeenCalledTimes(1));
    // Bcc must never feed the co-recipient graph (privacy).
    expect(recordSend).toHaveBeenCalledWith([
      "alice@example.com",
      "carol@example.com",
    ]);
  });

  it("derives the co-recipient anchor from a quoted display name with a comma", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    getCoRecipients.mockClear();

    const user = userEvent.setup();
    renderCompose();

    await screen.findByRole("button", { name: /^send$/i });
    // A naive `to.split(",")` would shred this into `"Smith` (no @) and
    // suppress the lookup entirely. The anchor must be the parsed email.
    await user.type(
      screen.getByLabelText(/^to/i),
      '"Smith, John" <john@example.com>',
    );

    await waitFor(() =>
      expect(getCoRecipients).toHaveBeenCalledWith("john@example.com", [
        "john@example.com",
      ]),
    );
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
      scheduledSendId: null,
      scheduledSendAt: null,
    });
    recordSend.mockClear();

    const user = userEvent.setup();
    renderCompose();
    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.click(screen.getByRole("button", { name: /^send$/i }));

    const undoBtn = await screen.findByTestId("undo-send-cancel");
    expect(undoBtn).toBeEnabled();
    // While the hold is live the recipients must NOT be recorded yet —
    // the send is still cancellable.
    expect(recordSend).not.toHaveBeenCalled();
    // Deadline expires within the timeout: banner disappears,
    // success toast appears, and the route navigates to /mail.
    await waitFor(
      () => expect(screen.queryByTestId("undo-send-cancel")).toBeNull(),
      { timeout: 3000 },
    );
    await screen.findByText(/back to mail/i);
    // Now that the hold elapsed the send is irrevocable, so the
    // recipients are recorded exactly once.
    await waitFor(() => expect(recordSend).toHaveBeenCalledTimes(1));
    expect(recordSend).toHaveBeenCalledWith(["alice@example.com"]);
  });

  it("cancels the pending send when the Undo button is clicked", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    sendEmail.mockResolvedValueOnce({
      emailId: "e-sent-cancel",
      pendingSendId: "ps-2",
      undoDeadline: new Date(Date.now() + 60_000),
      scheduledSendId: null,
      scheduledSendAt: null,
    });
    cancelPendingSend.mockResolvedValueOnce({ cancelled: true });
    recordSend.mockClear();

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
    // The send was cancelled during the hold, so its recipients must
    // never enter the frequent-contacts / co-recipient graph.
    expect(recordSend).not.toHaveBeenCalled();
  });

  it("sends the X-KMail-Schedule-At opt-in when the user picks 'Schedule for later'", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);
    const scheduledFor = new Date(Date.now() + 2 * 60 * 60 * 1000);
    sendEmail.mockResolvedValueOnce({
      emailId: "e-sent-sched",
      pendingSendId: null,
      undoDeadline: null,
      scheduledSendId: "ss-1",
      scheduledSendAt: scheduledFor,
    });

    const user = userEvent.setup();
    renderCompose();

    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.selectOptions(screen.getByTestId("compose-send-mode"), "schedule");
    // Default value is already 1h ahead — the picker uses the
    // local-ISO format, so just submit and trust the helper.
    await user.click(screen.getByTestId("compose-send"));

    await waitFor(() => expect(sendEmail).toHaveBeenCalledTimes(1));
    const [, , options] = sendEmail.mock.calls[0];
    expect(options).toMatchObject({ undoSend: false });
    expect(options.scheduleAt).toBeInstanceOf(Date);
    // Confirmation banner appears with a link to the scheduled list.
    expect(
      await screen.findByTestId("scheduled-send-confirm"),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /view scheduled sends/i }),
    ).toBeInTheDocument();
  });

  it("rejects a scheduled time that is in the past", async () => {
    getMailboxes.mockResolvedValueOnce(mailboxes);
    getIdentities.mockResolvedValueOnce(identities);

    const user = userEvent.setup();
    renderCompose();

    await screen.findByRole("button", { name: /^send$/i });
    await user.type(screen.getByLabelText(/^to/i), "alice@example.com");
    await user.selectOptions(screen.getByTestId("compose-send-mode"), "schedule");
    // Force an obviously-past value on the picker. userEvent.clear
    // + type on a datetime-local input is brittle across timezones,
    // so we set it via fireEvent on the underlying input.
    const picker = screen.getByTestId("compose-schedule-at") as HTMLInputElement;
    fireEvent.change(picker, { target: { value: "2000-01-01T00:00" } });

    await user.click(screen.getByTestId("compose-send"));
    expect(sendEmail).not.toHaveBeenCalled();
    expect(
      await screen.findByText(/at least 1 minute in the future/i),
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
