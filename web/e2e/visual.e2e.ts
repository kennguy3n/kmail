/**
 * Step 5 — Visual regression snapshots for key pages.
 *
 * These reuse the same MSW mock layer and fixtures as `make
 * screenshots` (`scripts/capture-screenshots.mjs`) but turn the
 * captures into assertions via Playwright's `toHaveScreenshot()`.
 *
 * Determinism notes:
 *  - The browser clock is pinned to the mock "now" (2026-04-28) so
 *    relative timestamps ("2h ago") and the calendar's default window
 *    render identically on every run.
 *  - Animations and the text caret are disabled, and a small pixel
 *    tolerance absorbs sub-pixel font hinting differences.
 *
 * Baselines are committed under `e2e/visual.e2e.ts-snapshots/`.
 * Regenerate intentionally with:
 *   npx playwright test e2e/visual.e2e.ts --update-snapshots
 */
import { test, expect, type Page } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

const MOCK_NOW = new Date("2026-04-28T12:00:00.000Z");

const snapshotOptions = {
  animations: "disabled" as const,
  caret: "hide" as const,
  fullPage: true,
  maxDiffPixelRatio: 0.02,
};

async function settle(page: Page): Promise<void> {
  // Let MSW responses resolve and React paint the resulting state.
  await page.waitForLoadState("networkidle");
}

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(MOCK_NOW);
});

test("inbox visual", async ({ page }) => {
  await gotoMocked(page, "/mail");
  await expect(page.getByRole("button", { name: /Welcome to KMail!/ })).toBeVisible();
  await settle(page);
  await expect(page).toHaveScreenshot("inbox.png", snapshotOptions);
});

test("compose visual", async ({ page }) => {
  await gotoMocked(page, "/mail/compose");
  await expect(page.getByRole("heading", { name: "New message" })).toBeVisible();
  await settle(page);
  await expect(page).toHaveScreenshot("compose.png", snapshotOptions);
});

test("calendar visual", async ({ page }) => {
  await gotoMocked(page, "/calendar");
  await expect(page.getByRole("heading", { name: "Calendar" })).toBeVisible();
  await settle(page);
  await expect(page).toHaveScreenshot("calendar.png", snapshotOptions);
});

test("user admin visual", async ({ page }) => {
  await gotoMocked(page, "/admin/users");
  await expect(page.getByRole("heading", { name: "User admin" })).toBeVisible();
  await selectTenant(page);
  await expect(page.getByRole("row", { name: /alice@acme\.com/ })).toBeVisible();
  await settle(page);
  await expect(page).toHaveScreenshot("user-admin.png", snapshotOptions);
});

test("security settings visual", async ({ page }) => {
  await gotoMocked(page, "/admin/security");
  await expect(page.getByRole("heading", { name: "Security" })).toBeVisible();
  await selectTenant(page);
  await settle(page);
  await expect(page).toHaveScreenshot("security-settings.png", snapshotOptions);
});
