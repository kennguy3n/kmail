/**
 * Unit tests for the theme store / `useTheme` hook.
 *
 * Verifies preference persistence, the resolved theme written to
 * `data-theme`, the light/dark toggle, and graceful handling of a
 * missing / "system" preference.
 */
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  THEME_STORAGE_KEY,
  initTheme,
  useTheme,
} from "./useTheme";

function setSystemDark(dark: boolean): void {
  vi.stubGlobal(
    "matchMedia",
    vi.fn().mockImplementation((query: string) => ({
      matches: dark && query.includes("dark"),
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  );
}

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  setSystemDark(false);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("useTheme", () => {
  it("defaults to system preference and resolves to light", () => {
    setSystemDark(false);
    initTheme();
    const { result } = renderHook(() => useTheme());
    expect(result.current.preference).toBe("system");
    expect(result.current.resolvedTheme).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("resolves system preference to dark when the OS prefers dark", () => {
    setSystemDark(true);
    initTheme();
    const { result } = renderHook(() => useTheme());
    expect(result.current.resolvedTheme).toBe("dark");
  });

  it("persists an explicit preference and applies it to <html>", () => {
    const { result } = renderHook(() => useTheme());
    act(() => result.current.setPreference("dark"));
    expect(result.current.preference).toBe("dark");
    expect(result.current.resolvedTheme).toBe("dark");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("toggles between light and dark based on the resolved theme", () => {
    const { result } = renderHook(() => useTheme());
    act(() => result.current.setPreference("light"));
    act(() => result.current.toggleTheme());
    expect(result.current.resolvedTheme).toBe("dark");
    act(() => result.current.toggleTheme());
    expect(result.current.resolvedTheme).toBe("light");
  });

  it("reads an existing stored preference on init", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "dark");
    initTheme();
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });
});
