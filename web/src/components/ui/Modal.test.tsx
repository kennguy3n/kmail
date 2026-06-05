/**
 * Unit tests for the Modal dialog.
 *
 * Covers conditional rendering, the dialog a11y contract, Escape /
 * overlay / close-button dismissal, and focus restoration to the
 * trigger on close.
 */
import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Modal } from "./Modal";

function Harness(): JSX.Element {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button onClick={() => setOpen(true)}>Open</button>
      <Modal open={open} onClose={() => setOpen(false)} title="My dialog">
        <button>Inside</button>
      </Modal>
    </>
  );
}

describe("Modal", () => {
  it("does not render when closed", () => {
    render(
      <Modal open={false} onClose={vi.fn()} title="Hidden">
        body
      </Modal>,
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("renders an accessible dialog labelled by its title", () => {
    render(
      <Modal open onClose={vi.fn()} title="My dialog">
        body
      </Modal>,
    );
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAccessibleName("My dialog");
  });

  it("closes on Escape", async () => {
    const onClose = vi.fn();
    render(
      <Modal open onClose={onClose} title="X">
        body
      </Modal>,
    );
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("closes via the close button", async () => {
    const onClose = vi.fn();
    render(
      <Modal open onClose={onClose} title="X">
        body
      </Modal>,
    );
    await userEvent.click(screen.getByRole("button", { name: /close dialog/i }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("restores focus to the trigger after closing", async () => {
    render(<Harness />);
    const openBtn = screen.getByRole("button", { name: "Open" });
    await userEvent.click(openBtn);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(openBtn).toHaveFocus();
  });
});
