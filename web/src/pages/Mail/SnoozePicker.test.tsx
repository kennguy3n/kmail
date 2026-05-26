/**
 * Component test for SnoozePicker.tsx.
 *
 * Pins:
 *   - Each preset resolves to a future Date that's strictly
 *     greater than the injected `now`.
 *   - The custom datetime-local input passes the chosen value to
 *     onPick.
 *   - In-the-past values surface an inline error and do NOT call
 *     onPick (so the user can correct before round-tripping the
 *     400 from the BFF).
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import SnoozePicker from "./SnoozePicker";

const NOW = new Date("2026-06-15T10:00:00Z"); // Mon

function fixedNow() {
  return NOW;
}

describe("<SnoozePicker />", () => {
  it("invokes onPick with a future Date when 'Later today' is chosen", async () => {
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(
      <SnoozePicker
        onPick={onPick}
        onCancel={() => {}}
        now={fixedNow}
      />,
    );
    await user.click(screen.getByText(/later today/i));
    expect(onPick).toHaveBeenCalledTimes(1);
    const arg = onPick.mock.calls[0][0] as Date;
    expect(arg.getTime()).toBeGreaterThan(NOW.getTime());
  });

  it("invokes onPick with a future Date when 'Tomorrow morning' is chosen", async () => {
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(
      <SnoozePicker onPick={onPick} onCancel={() => {}} now={fixedNow} />,
    );
    await user.click(screen.getByText(/tomorrow morning/i));
    expect(onPick).toHaveBeenCalledTimes(1);
    const arg = onPick.mock.calls[0][0] as Date;
    expect(arg.getTime()).toBeGreaterThan(NOW.getTime());
  });

  it("uses the custom datetime input when 'Pick' is clicked", async () => {
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(
      <SnoozePicker onPick={onPick} onCancel={() => {}} now={fixedNow} />,
    );
    const input = screen.getByLabelText(/custom snooze date and time/i);
    // datetime-local is a wall-clock string; we pick something well
    // past `NOW` to guarantee acceptance.
    fireEvent.change(input, { target: { value: "2026-06-15T18:00" } });
    await user.click(screen.getByRole("button", { name: /pick/i }));
    expect(onPick).toHaveBeenCalledTimes(1);
  });

  it("rejects a custom time that is in the past with an inline error", async () => {
    const onPick = vi.fn();
    const user = userEvent.setup();
    render(
      <SnoozePicker onPick={onPick} onCancel={() => {}} now={fixedNow} />,
    );
    const input = screen.getByLabelText(/custom snooze date and time/i);
    fireEvent.change(input, { target: { value: "2000-01-01T00:00" } });
    await user.click(screen.getByRole("button", { name: /pick/i }));
    expect(onPick).not.toHaveBeenCalled();
    expect(
      screen.getByText(/snooze must be at least one minute away/i),
    ).toBeInTheDocument();
  });

  it("Cancel button invokes onCancel without picking", async () => {
    const onPick = vi.fn();
    const onCancel = vi.fn();
    const user = userEvent.setup();
    render(
      <SnoozePicker onPick={onPick} onCancel={onCancel} now={fixedNow} />,
    );
    await user.click(screen.getByRole("button", { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onPick).not.toHaveBeenCalled();
  });
});
