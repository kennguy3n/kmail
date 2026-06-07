/**
 * Unit tests for the EmptyState placeholder.
 *
 * Verifies the title/description render, the decorative icon is
 * hidden from assistive tech, and an optional action (e.g. a CTA
 * button) is rendered and interactive.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { EmptyState } from "./EmptyState";

describe("EmptyState", () => {
  it("renders the title and description", () => {
    render(
      <EmptyState title="No messages" description="Your inbox is empty." />,
    );
    expect(screen.getByText("No messages")).toBeInTheDocument();
    expect(screen.getByText("Your inbox is empty.")).toBeInTheDocument();
  });

  it("hides the decorative icon from assistive tech", () => {
    const { container } = render(
      <EmptyState icon={<svg data-testid="icon" />} title="Empty" />,
    );
    const iconWrap = container.querySelector("[aria-hidden='true']");
    expect(iconWrap).not.toBeNull();
    expect(iconWrap).toContainElement(screen.getByTestId("icon"));
  });

  it("renders an interactive action", async () => {
    const onClick = vi.fn();
    render(
      <EmptyState
        title="Empty"
        action={<button onClick={onClick}>Compose</button>}
      />,
    );
    await userEvent.click(screen.getByRole("button", { name: "Compose" }));
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});
