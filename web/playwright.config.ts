import { defineConfig, devices } from "@playwright/test";

/**
 * Playwright configuration for the KMail web E2E suite.
 *
 * The specs in `e2e/` drive the real React SPA against the MSW mock
 * layer (`src/mocks/`) — the same fixtures `make screenshots` uses.
 * The dev server is started with `VITE_MOCK_API=true` so `main.tsx`
 * boots the service worker before mounting React, letting every
 * `/jmap` and `/api/v1/*` call resolve from the in-browser mock
 * without the Go BFF running.
 *
 * Tests query by role / accessible name / label rather than CSS
 * classes so they survive the in-flight Tailwind + Radix migration
 * (branch `ui/design-system-migration`).
 */
const PORT = 5173;
const BASE_URL = `http://localhost:${PORT}`;

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.e2e.ts",
  // Each spec walks an independent golden path; run them in parallel.
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: [["html", { open: "never" }], ["list"]],
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: BASE_URL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      // Spread the device preset first, then pin our deterministic overrides:
      // project `use` merges over the global `use`, and `devices["Desktop
      // Chrome"]` carries its own values (e.g. a 1280x720 viewport) that would
      // otherwise win. Order matters — these explicit fields must come *after*
      // the spread so they take effect even if a future Playwright version adds
      // them to the device descriptor:
      //   - viewport: the fixed 1440x900 frame the visual-regression baselines
      //     were captured at.
      //   - locale: en-US, so number/date formatting is deterministic across
      //     machines. The user-quota spec compares the rendered cell against a
      //     value formatted with `toLocaleString("en-US")` in Node
      //     (`06-admin-user-quota.e2e.ts`); without this it would depend on
      //     whatever default locale the runner's Chromium happens to use.
      use: {
        ...devices["Desktop Chrome"],
        viewport: { width: 1440, height: 900 },
        locale: "en-US",
      },
    },
  ],
  webServer: {
    command: "npm run dev -- --port 5173 --strictPort",
    url: BASE_URL,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
    env: { VITE_MOCK_API: "true" },
  },
});
