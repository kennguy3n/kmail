/**
 * Unit tests for the Skeleton loading placeholder.
 *
 * A bare Skeleton is decorative and must be hidden from assistive
 * tech; passing a `label` promotes it to a `role="status"` live
 * region that announces the loading state exactly once. Multi-line
 * skeletons render one bar per line.
 */
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { Skeleton } from "./Skeleton";

describe("Skeleton", () => {
  it("is decorative (aria-hidden) without a label", () => {
    const { container } = render(<Skeleton width="8rem" height="1rem" />);
    // No status region is announced for a bare placeholder.
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    const bar = container.querySelector("[aria-hidden='true']");
    expect(bar).not.toBeNull();
  });

  it("exposes a labelled status region when given a label", () => {
    render(<Skeleton label="Loading messages" />);
    const status = screen.getByRole("status");
    expect(status).toHaveAttribute("aria-busy", "true");
    expect(status).toHaveTextContent("Loading messages");
  });

  it("renders one bar per line for multi-line placeholders", () => {
    const { container } = render(<Skeleton lines={3} />);
    // 3 stacked bars (each its own span with inline styles).
    const bars = container.querySelectorAll("span[style]");
    expect(bars.length).toBe(3);
  });
});
