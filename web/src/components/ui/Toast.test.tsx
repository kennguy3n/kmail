/**
 * Unit tests for the Toast notification card.
 *
 * The load-bearing a11y behaviour is the live-region role: error and
 * warning toasts are assertive (`role="alert"`) so screen readers
 * interrupt, while success/info are polite (`role="status"`). Each
 * toast also exposes a labelled dismiss button.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Toast } from "./Toast";
import type { ToastData, ToastVariant } from "./Toast";

function makeToast(overrides: Partial<ToastData> = {}): ToastData {
  return {
    id: "t1",
    description: "Saved",
    variant: "success",
    duration: 0,
    ...overrides,
  };
}

describe("Toast", () => {
  it("renders the description and optional title", () => {
    render(
      <Toast
        toast={makeToast({ title: "Heads up", description: "All good" })}
        onDismiss={vi.fn()}
      />,
    );
    expect(screen.getByText("Heads up")).toBeInTheDocument();
    expect(screen.getByText("All good")).toBeInTheDocument();
  });

  it.each<[ToastVariant, string]>([
    ["error", "alert"],
    ["warning", "alert"],
    ["success", "status"],
    ["info", "status"],
  ])("uses role=%s region semantics for the %s variant", (variant, role) => {
    render(<Toast toast={makeToast({ variant })} onDismiss={vi.fn()} />);
    expect(screen.getByRole(role)).toBeInTheDocument();
  });

  it("invokes onDismiss with the toast id from the close button", async () => {
    const onDismiss = vi.fn();
    render(<Toast toast={makeToast({ id: "abc" })} onDismiss={onDismiss} />);
    await userEvent.click(
      screen.getByRole("button", { name: /dismiss notification/i }),
    );
    expect(onDismiss).toHaveBeenCalledWith("abc");
  });
});
