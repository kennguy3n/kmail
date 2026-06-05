/**
 * Unit tests for the keyboard-shortcut engine.
 *
 * Covers single-key matches, multi-step sequences ("g i"), the
 * input-focus guard, the modifier-chord guard, and the `enabled`
 * switch.
 */
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import {
  useKeyboardShortcuts,
  type KeyboardShortcut,
} from "./useKeyboardShortcuts";

function Harness({
  shortcuts,
  enabled,
}: {
  shortcuts: KeyboardShortcut[];
  enabled?: boolean;
}): JSX.Element {
  useKeyboardShortcuts(shortcuts, { enabled });
  return (
    <div>
      <input aria-label="text" />
      <textarea aria-label="area" />
    </div>
  );
}

describe("useKeyboardShortcuts", () => {
  it("fires a single-key shortcut", async () => {
    const handler = vi.fn();
    render(<Harness shortcuts={[{ keys: "c", description: "", handler }]} />);
    await userEvent.keyboard("c");
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("fires a two-step sequence in order", async () => {
    const handler = vi.fn();
    render(
      <Harness shortcuts={[{ keys: "g i", description: "", handler }]} />,
    );
    await userEvent.keyboard("gi");
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("does not fire the sequence for a lone first key", async () => {
    const handler = vi.fn();
    render(
      <Harness shortcuts={[{ keys: "g i", description: "", handler }]} />,
    );
    await userEvent.keyboard("g");
    expect(handler).not.toHaveBeenCalled();
  });

  it("ignores shortcuts while a text field is focused", async () => {
    const handler = vi.fn();
    const { getByLabelText } = render(
      <Harness shortcuts={[{ keys: "c", description: "", handler }]} />,
    );
    (getByLabelText("text") as HTMLInputElement).focus();
    await userEvent.keyboard("c");
    expect(handler).not.toHaveBeenCalled();
  });

  it("honours allowInInput", async () => {
    const handler = vi.fn();
    const { getByLabelText } = render(
      <Harness
        shortcuts={[
          { keys: "c", description: "", handler, allowInInput: true },
        ]}
      />,
    );
    (getByLabelText("text") as HTMLInputElement).focus();
    await userEvent.keyboard("c");
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("ignores modifier chords (Ctrl/Meta/Alt)", async () => {
    const handler = vi.fn();
    render(<Harness shortcuts={[{ keys: "c", description: "", handler }]} />);
    await userEvent.keyboard("{Control>}c{/Control}");
    expect(handler).not.toHaveBeenCalled();
  });

  it("does nothing when disabled", async () => {
    const handler = vi.fn();
    render(
      <Harness
        enabled={false}
        shortcuts={[{ keys: "c", description: "", handler }]}
      />,
    );
    await userEvent.keyboard("c");
    expect(handler).not.toHaveBeenCalled();
  });

  it("matches symbol shortcuts like ?", async () => {
    const handler = vi.fn();
    render(<Harness shortcuts={[{ keys: "?", description: "", handler }]} />);
    await userEvent.keyboard("?");
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("prefers the longest match so a single key never shadows a sequence", async () => {
    const single = vi.fn();
    const sequence = vi.fn();
    // Single-key "c" registered BEFORE the "g c" sequence — the engine
    // must still route "g c" to the sequence regardless of order.
    render(
      <Harness
        shortcuts={[
          { keys: "c", description: "", handler: single },
          { keys: "g c", description: "", handler: sequence },
        ]}
      />,
    );

    await userEvent.keyboard("gc");
    expect(sequence).toHaveBeenCalledTimes(1);
    expect(single).not.toHaveBeenCalled();

    // A lone "c" still fires the single-key shortcut.
    await userEvent.keyboard("c");
    expect(single).toHaveBeenCalledTimes(1);
    expect(sequence).toHaveBeenCalledTimes(1);
  });
});
