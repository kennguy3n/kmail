/**
 * Dark/light token-switching tests for the design system.
 *
 * Every CSS custom property in `styles/global.css` keys off the
 * `data-theme` attribute on `<html>`, so the observable contract of
 * "token switching" is: setting an explicit preference flips
 * `document.documentElement[data-theme]` between "light" and "dark"
 * (and persists the choice), and components mounted under either
 * theme render the same accessible markup (they are theme-agnostic
 * at the DOM level — only the resolved tokens change).
 *
 * We drive this through the real `useTheme` store rather than a mock
 * so the test exercises the same code path the header toggle uses.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { THEME_STORAGE_KEY, useTheme } from "../../hooks/useTheme";
import { Button } from "./Button";
import { Card } from "./Card";

function ThemeHarness(): JSX.Element {
  const { resolvedTheme, toggleTheme, setPreference } = useTheme();
  return (
    <div>
      <span data-testid="resolved">{resolvedTheme}</span>
      <button onClick={toggleTheme}>toggle</button>
      <button onClick={() => setPreference("light")}>light</button>
      <button onClick={() => setPreference("dark")}>dark</button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
});

afterEach(() => {
  localStorage.clear();
});

describe("theme token switching", () => {
  it("writes the resolved theme onto <html data-theme> when set to light", async () => {
    render(<ThemeHarness />);
    await userEvent.click(screen.getByRole("button", { name: "light" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(screen.getByTestId("resolved")).toHaveTextContent("light");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
  });

  it("flips between light and dark and persists the explicit choice", async () => {
    render(<ThemeHarness />);
    await userEvent.click(screen.getByRole("button", { name: "dark" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");

    await userEvent.click(screen.getByRole("button", { name: "toggle" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
  });

  it("renders the same accessible markup under both themes", async () => {
    function Sample(): JSX.Element {
      return (
        <Card title="Settings">
          <Button>Save</Button>
        </Card>
      );
    }

    const { rerender } = render(
      <>
        <ThemeHarness />
        <Sample />
      </>,
    );

    await userEvent.click(screen.getByRole("button", { name: "dark" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "light" }));
    rerender(
      <>
        <ThemeHarness />
        <Sample />
      </>,
    );
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    // Same roles/names regardless of the active theme tokens.
    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });
});
