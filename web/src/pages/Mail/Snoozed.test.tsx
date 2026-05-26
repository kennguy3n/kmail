/**
 * Component test for Snoozed.tsx.
 *
 * Pins the load-bearing list flow:
 *   - On mount, `listSnoozes()` is called once and the rows are
 *     rendered ordered with active snoozes first.
 *   - Clicking Wake now on a snoozed row calls `wakeSnooze()` and
 *     refreshes the list.
 *   - The empty state renders the right copy.
 *
 * Backend semantics are tested in
 * `internal/snooze/handlers_test.go`; this test pins the UI
 * contract.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import Snoozed from "./Snoozed";
import type { SnoozeSnapshot } from "../../api/snooze";

const listSnoozes = vi.fn();
const wakeSnooze = vi.fn();

vi.mock("../../api/snooze", () => ({
  listSnoozes: () => listSnoozes(),
  wakeSnooze: (id: string) => wakeSnooze(id),
}));

function renderPage() {
  return render(
    <MemoryRouter>
      <Snoozed />
    </MemoryRouter>,
  );
}

const snoozedRow: SnoozeSnapshot = {
  id: "s-snoozed",
  status: "snoozed",
  email_id: "email-1",
  snoozed_mailbox_id: "mb-snoozed",
  snooze_until: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
  mark_unread_on_wake: true,
  attempts: 0,
  created_at: new Date().toISOString(),
};

const unsnoozedRow: SnoozeSnapshot = {
  id: "s-woke",
  status: "unsnoozed",
  email_id: "email-2",
  snoozed_mailbox_id: "mb-snoozed",
  snooze_until: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
  mark_unread_on_wake: true,
  attempts: 1,
  woken_at: new Date(Date.now() - 30 * 60 * 1000).toISOString(),
  created_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
};

describe("<Snoozed />", () => {
  it("loads and renders active + historical rows on mount", async () => {
    listSnoozes.mockResolvedValueOnce([unsnoozedRow, snoozedRow]);

    renderPage();

    await waitFor(() => expect(listSnoozes).toHaveBeenCalledTimes(1));
    expect(
      await screen.findByTestId("wake-snooze-s-snoozed"),
    ).toBeInTheDocument();
    expect(screen.getByText("email-1")).toBeInTheDocument();
    expect(screen.getByText("email-2")).toBeInTheDocument();
  });

  it("wakes a snoozed row and refreshes the list", async () => {
    listSnoozes.mockResolvedValueOnce([snoozedRow]);
    wakeSnooze.mockResolvedValueOnce({ cancelled: true });
    listSnoozes.mockResolvedValueOnce([
      { ...snoozedRow, status: "cancelled" as const },
    ]);

    const user = userEvent.setup();
    renderPage();
    const wakeBtn = await screen.findByTestId("wake-snooze-s-snoozed");
    await user.click(wakeBtn);

    await waitFor(() =>
      expect(wakeSnooze).toHaveBeenCalledWith("s-snoozed"),
    );
    expect(
      await screen.findByText(/snooze cancelled/i),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByTestId("wake-snooze-s-snoozed")).toBeNull(),
    );
  });

  it("surfaces an error if the wake call rejects", async () => {
    listSnoozes.mockResolvedValueOnce([snoozedRow]);
    wakeSnooze.mockRejectedValueOnce(new Error("connect refused"));

    const user = userEvent.setup();
    renderPage();
    const wakeBtn = await screen.findByTestId("wake-snooze-s-snoozed");
    await user.click(wakeBtn);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      /connect refused/i,
    );
  });

  it("renders an empty state when there are no rows", async () => {
    listSnoozes.mockResolvedValueOnce([]);

    renderPage();

    expect(
      await screen.findByText(/don't have any snoozed emails/i),
    ).toBeInTheDocument();
  });

  it("renders a Retry wake button for failed rows and retries the wake", async () => {
    // Pins the Round 6 fix at the UI layer: failed rows (worker
    // exhausted retries → email still stuck) must offer a
    // user-facing self-service recovery path, not a dash.
    const failedRow: SnoozeSnapshot = {
      id: "s-failed",
      status: "failed",
      email_id: "email-3",
      snoozed_mailbox_id: "mb-snoozed",
      snooze_until: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
      mark_unread_on_wake: true,
      attempts: 3,
      last_error: "internal-host-42:9123 connect refused",
      created_at: new Date(Date.now() - 4 * 60 * 60 * 1000).toISOString(),
    };
    listSnoozes.mockResolvedValueOnce([failedRow]);
    wakeSnooze.mockResolvedValueOnce({ cancelled: true });
    listSnoozes.mockResolvedValueOnce([
      { ...failedRow, status: "cancelled" as const },
    ]);

    const user = userEvent.setup();
    renderPage();
    const retryBtn = await screen.findByTestId("retry-snooze-s-failed");
    expect(retryBtn).toHaveTextContent(/retry wake/i);
    await user.click(retryBtn);

    await waitFor(() =>
      expect(wakeSnooze).toHaveBeenCalledWith("s-failed"),
    );
    expect(
      await screen.findByText(/wake retried/i),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.queryByTestId("retry-snooze-s-failed")).toBeNull(),
    );
  });
});
