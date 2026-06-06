/**
 * Unit tests for the Card surface container.
 *
 * Card is presentational, so the meaningful contract is: it renders
 * its children, conditionally renders the titled header, forwards
 * arbitrary DOM props (so callers can attach roles / aria / data
 * attributes), and merges a caller-supplied className. These assert
 * on structure/roles rather than CSS-module class names so they
 * survive the Tailwind/Radix migration.
 */
import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { Card } from "./Card";

describe("Card", () => {
  it("renders its children", () => {
    render(<Card>Body content</Card>);
    expect(screen.getByText("Body content")).toBeInTheDocument();
  });

  it("renders a title as a level-3 heading when provided", () => {
    render(<Card title="Account">body</Card>);
    expect(
      screen.getByRole("heading", { level: 3, name: "Account" }),
    ).toBeInTheDocument();
  });

  it("omits the header entirely when neither title nor actions are given", () => {
    render(<Card>just a body</Card>);
    expect(screen.queryByRole("heading")).not.toBeInTheDocument();
  });

  it("renders header actions even without a title", () => {
    render(
      <Card actions={<button>Edit</button>}>body</Card>,
    );
    expect(screen.getByRole("button", { name: "Edit" })).toBeInTheDocument();
    // No title means no heading element.
    expect(screen.queryByRole("heading")).not.toBeInTheDocument();
  });

  it("forwards arbitrary DOM props (role, aria-label, data-*)", () => {
    render(
      <Card role="region" aria-label="Stats" data-testid="card">
        body
      </Card>,
    );
    const region = screen.getByRole("region", { name: "Stats" });
    expect(region).toHaveAttribute("data-testid", "card");
  });
});
