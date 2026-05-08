/**
 * Vitest global setup.
 *
 * Loaded once per test runner via `vitest.config.ts#test.setupFiles`.
 * Registers `@testing-library/jest-dom` matchers and resets the
 * global `fetch` mock between tests so each test starts from a
 * known empty state.
 */
import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

beforeEach(() => {
  // Default fetch mock — individual tests override this with
  // `vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(...)` to
  // assert on the request shape and the parsed response body.
  vi.stubGlobal(
    "fetch",
    vi.fn(() => {
      throw new Error(
        "kmail-web tests: unexpected fetch — stub it explicitly",
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
