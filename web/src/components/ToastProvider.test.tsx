/**
 * Unit tests for the Toast notification system.
 *
 * Covers raising each variant, manual dismissal, auto-dismiss via
 * timers, the max-visible cap, and the `useToast`-outside-provider
 * guard.
 */
import { act, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { ToastProvider, useToast } from "./ToastProvider";

function wrapper({ children }: { children: React.ReactNode }): JSX.Element {
  return <ToastProvider max={3}>{children}</ToastProvider>;
}

describe("ToastProvider / useToast", () => {
  it("throws when used outside a provider", () => {
    expect(() => renderHook(() => useToast())).toThrowError(
      /within a <ToastProvider>/,
    );
  });

  it("shows a success toast with status role", () => {
    const { result } = renderHook(() => useToast(), { wrapper });
    act(() => {
      result.current.success("Saved");
    });
    const toast = screen.getByText("Saved");
    expect(toast).toBeInTheDocument();
    expect(screen.getByRole("status")).toBeInTheDocument();
  });

  it("renders error toasts with assertive alert role", () => {
    const { result } = renderHook(() => useToast(), { wrapper });
    act(() => {
      result.current.error("Boom");
    });
    expect(screen.getByRole("alert")).toHaveTextContent("Boom");
  });

  it("dismisses a toast manually via the close button", async () => {
    const { result } = renderHook(() => useToast(), { wrapper });
    act(() => {
      result.current.info("Closable");
    });
    expect(screen.getByText("Closable")).toBeInTheDocument();
    await userEvent.click(
      screen.getByRole("button", { name: /dismiss notification/i }),
    );
    expect(screen.queryByText("Closable")).not.toBeInTheDocument();
  });

  it("auto-dismisses after the configured duration", () => {
    vi.useFakeTimers();
    try {
      const { result } = renderHook(() => useToast(), { wrapper });
      act(() => {
        result.current.info("Temporary", { duration: 1000 });
      });
      expect(screen.getByText("Temporary")).toBeInTheDocument();
      act(() => {
        vi.advanceTimersByTime(1000);
      });
      expect(screen.queryByText("Temporary")).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("caps the number of visible toasts, dropping the oldest", () => {
    const { result } = renderHook(() => useToast(), { wrapper });
    act(() => {
      result.current.info("one", { duration: 0 });
      result.current.info("two", { duration: 0 });
      result.current.info("three", { duration: 0 });
      result.current.info("four", { duration: 0 });
    });
    expect(screen.queryByText("one")).not.toBeInTheDocument();
    expect(screen.getByText("two")).toBeInTheDocument();
    expect(screen.getByText("four")).toBeInTheDocument();
  });
});
