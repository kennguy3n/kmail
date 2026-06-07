/**
 * Unit tests for the Tooltip primitive.
 *
 * The accessibility contract is the important part: the trigger is
 * linked to the tooltip via `aria-describedby` only while visible,
 * the tooltip shows on hover and on keyboard focus (not just mouse),
 * and it is dismissible with Escape per WCAG 1.4.13 without removing
 * focus from the trigger.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { Tooltip } from "./Tooltip";

describe("Tooltip", () => {
  it("is hidden until the trigger is hovered or focused", () => {
    render(
      <Tooltip label="More info">
        <button>Help</button>
      </Tooltip>,
    );
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Help" })).not.toHaveAttribute(
      "aria-describedby",
    );
  });

  it("shows on hover and links the trigger via aria-describedby", async () => {
    render(
      <Tooltip label="More info">
        <button>Help</button>
      </Tooltip>,
    );
    const trigger = screen.getByRole("button", { name: "Help" });
    await userEvent.hover(trigger);

    const tip = screen.getByRole("tooltip");
    expect(tip).toHaveTextContent("More info");
    expect(trigger).toHaveAttribute("aria-describedby", tip.id);

    await userEvent.unhover(trigger);
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  });

  it("shows on keyboard focus (not only mouse)", async () => {
    render(
      <Tooltip label="Keyboard reachable">
        <button>Help</button>
      </Tooltip>,
    );
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Help" })).toHaveFocus();
    expect(screen.getByRole("tooltip")).toHaveTextContent("Keyboard reachable");
  });

  it("dismisses on Escape while keeping focus on the trigger", async () => {
    render(
      <Tooltip label="Dismiss me">
        <button>Help</button>
      </Tooltip>,
    );
    const trigger = screen.getByRole("button", { name: "Help" });
    await userEvent.tab();
    expect(screen.getByRole("tooltip")).toBeInTheDocument();

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it("preserves the child's own event handlers", async () => {
    let clicked = false;
    render(
      <Tooltip label="info">
        <button onClick={() => (clicked = true)}>Help</button>
      </Tooltip>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Help" }));
    expect(clicked).toBe(true);
  });
});
