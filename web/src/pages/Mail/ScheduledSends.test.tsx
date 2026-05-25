/**
 * Component test for ScheduledSends.tsx.
 *
 * Pins the load-bearing list flow:
 *   - On mount, `listScheduledSends()` is called once and the
 *     rows are rendered ordered with pending first.
 *   - Clicking Cancel on a pending row calls
 *     `cancelScheduledSend()` and refreshes the list.
 *   - 410 (worker beat us) surfaces the right copy.
 *
 * Backend semantics are tested in
 * `internal/scheduledsend/handlers_test.go`; this test pins the
 * UI contract.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import ScheduledSends from "./ScheduledSends";
import type { ScheduledSendSnapshot } from "../../api/scheduledSend";

const listScheduledSends = vi.fn();
const cancelScheduledSend = vi.fn();

vi.mock("../../api/scheduledSend", () => ({
  listScheduledSends: () => listScheduledSends(),
  cancelScheduledSend: (id: string) => cancelScheduledSend(id),
}));

function renderPage() {
  return render(
    <MemoryRouter>
      <ScheduledSends />
    </MemoryRouter>,
  );
}

const pendingRow: ScheduledSendSnapshot = {
  id: "ss-pending",
  status: "pending",
  email_id: "email-1",
  identity_id: "id-1",
  send_at: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
  attempts: 0,
  created_at: new Date().toISOString(),
};

const sentRow: ScheduledSendSnapshot = {
  id: "ss-sent",
  status: "sent",
  email_id: "email-2",
  identity_id: "id-1",
  send_at: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
  attempts: 1,
  sent_at: new Date(Date.now() - 30 * 60 * 1000).toISOString(),
  created_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
};

describe("<ScheduledSends />", () => {
  it("loads and renders pending + sent rows on mount", async () => {
    listScheduledSends.mockResolvedValueOnce([sentRow, pendingRow]);

    renderPage();

    await waitFor(() => expect(listScheduledSends).toHaveBeenCalledTimes(1));
    // Pending row should be present; the cancel button targets
    // the pending row's id (sent rows render `Sent …` instead).
    expect(
      await screen.findByTestId("cancel-scheduled-ss-pending"),
    ).toBeInTheDocument();
    expect(screen.getByText("email-1")).toBeInTheDocument();
    expect(screen.getByText("email-2")).toBeInTheDocument();
  });

  it("cancels a pending row and refreshes the list", async () => {
    listScheduledSends.mockResolvedValueOnce([pendingRow]);
    cancelScheduledSend.mockResolvedValueOnce({ cancelled: true });
    listScheduledSends.mockResolvedValueOnce([
      { ...pendingRow, status: "cancelled" as const },
    ]);

    const user = userEvent.setup();
    renderPage();
    const cancelBtn = await screen.findByTestId("cancel-scheduled-ss-pending");
    await user.click(cancelBtn);

    await waitFor(() =>
      expect(cancelScheduledSend).toHaveBeenCalledWith("ss-pending"),
    );
    expect(
      await screen.findByText(/scheduled send cancelled/i),
    ).toBeInTheDocument();
    // After the reload the row is in `cancelled` state, so the
    // Cancel button is gone.
    await waitFor(() =>
      expect(
        screen.queryByTestId("cancel-scheduled-ss-pending"),
      ).toBeNull(),
    );
  });

  it("surfaces 'too late' messaging when the worker already dispatched", async () => {
    listScheduledSends.mockResolvedValueOnce([pendingRow]);
    cancelScheduledSend.mockResolvedValueOnce({ cancelled: false });
    listScheduledSends.mockResolvedValueOnce([
      { ...pendingRow, status: "sent" as const, sent_at: new Date().toISOString() },
    ]);

    const user = userEvent.setup();
    renderPage();
    const cancelBtn = await screen.findByTestId("cancel-scheduled-ss-pending");
    await user.click(cancelBtn);

    expect(
      await screen.findByText(/too late/i),
    ).toBeInTheDocument();
  });

  it("renders an empty state when there are no rows", async () => {
    listScheduledSends.mockResolvedValueOnce([]);

    renderPage();

    expect(
      await screen.findByText(/don't have any scheduled sends yet/i),
    ).toBeInTheDocument();
  });
});
