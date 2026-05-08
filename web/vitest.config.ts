import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

/**
 * Vitest configuration for the KMail React frontend.
 *
 * - `jsdom` gives us a browser-shaped global so we can render
 *   components and assert on their DOM output without spinning up
 *   a real browser.
 * - `setupFiles` registers `@testing-library/jest-dom` matchers
 *   (`toBeInTheDocument`, `toHaveTextContent`, etc.) and the
 *   `fetch` mock reset hook used by the API client tests.
 * - `globals: true` exposes `describe` / `it` / `expect` /
 *   `vi` without per-file imports, matching the convention in
 *   the surrounding KChat codebase.
 *
 * `test.css` is intentionally `false` — none of the components we
 * test rely on CSS modules and parsing CSS for every test would
 * just slow the suite down.
 */
export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
