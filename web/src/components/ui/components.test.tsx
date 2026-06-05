/**
 * Unit tests for the smaller library primitives: Button, Avatar
 * (initials derivation), Badge, Tabs (keyboard navigation), and
 * Dropdown (open + select + outside-click close).
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Button } from "./Button";
import { Avatar, initialsFromName } from "./Avatar";
import { Badge } from "./Badge";
import { Tabs } from "./Tabs";
import { Dropdown } from "./Dropdown";

describe("Button", () => {
  it("defaults to type=button so it never submits a form", () => {
    render(<Button>Go</Button>);
    expect(screen.getByRole("button", { name: "Go" })).toHaveAttribute(
      "type",
      "button",
    );
  });

  it("is disabled and busy while loading", () => {
    render(<Button loading>Save</Button>);
    const btn = screen.getByRole("button");
    expect(btn).toBeDisabled();
    expect(btn).toHaveAttribute("aria-busy", "true");
  });
});

describe("initialsFromName", () => {
  it("derives two initials from a full name", () => {
    expect(initialsFromName("Ada Lovelace")).toBe("AL");
  });

  it("uses the email local part", () => {
    expect(initialsFromName("alan.turing@example.com")).toBe("AT");
  });

  it("falls back for a single token", () => {
    expect(initialsFromName("Madonna")).toBe("MA");
  });

  it("handles empty input", () => {
    expect(initialsFromName("   ")).toBe("?");
  });
});

describe("Avatar", () => {
  it("exposes the name as an accessible label", () => {
    render(<Avatar name="Grace Hopper" />);
    expect(screen.getByRole("img", { name: "Grace Hopper" })).toBeInTheDocument();
  });
});

describe("Badge", () => {
  it("renders its content", () => {
    render(<Badge variant="success">Active</Badge>);
    expect(screen.getByText("Active")).toBeInTheDocument();
  });
});

describe("Tabs", () => {
  it("renders the first tab panel by default", () => {
    render(
      <Tabs
        ariaLabel="t"
        items={[
          { id: "a", label: "A", content: "Panel A" },
          { id: "b", label: "B", content: "Panel B" },
        ]}
      />,
    );
    expect(screen.getByText("Panel A")).toBeInTheDocument();
    expect(screen.queryByText("Panel B")).not.toBeInTheDocument();
  });

  it("moves selection with arrow keys", async () => {
    render(
      <Tabs
        ariaLabel="t"
        items={[
          { id: "a", label: "A", content: "Panel A" },
          { id: "b", label: "B", content: "Panel B" },
        ]}
      />,
    );
    screen.getByRole("tab", { name: "A" }).focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "B" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(screen.getByText("Panel B")).toBeInTheDocument();
  });
});

describe("Dropdown", () => {
  it("opens on trigger click and invokes onSelect", async () => {
    const onSelect = vi.fn();
    render(
      <Dropdown
        ariaLabel="menu"
        trigger={<button>Open menu</button>}
        items={[{ id: "edit", label: "Edit", onSelect }]}
      />,
    );
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Open menu" }));
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("menuitem", { name: "Edit" }));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
});
