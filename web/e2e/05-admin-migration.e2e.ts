/**
 * Golden path 5 — Admin → migration wizard → test connection → start
 * import.
 *
 * Walks the three-step IMAP migration wizard: pick the generic IMAP
 * provider, enter credentials, run the connection probe (mock reports
 * success), then start the import. Starting the job POSTs to the
 * migrations endpoint and the wizard reloads the jobs table.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

test("run the IMAP migration wizard end to end", async ({ page }) => {
  await gotoMocked(page, "/admin/migrations");

  await expect(
    page.getByRole("heading", { name: "Migration wizard" }),
  ).toBeVisible();

  await selectTenant(page);

  // Step 1 — choose the generic IMAP provider.
  await page.getByRole("radio", { name: "generic imap" }).check();
  await page.getByRole("button", { name: /Next/ }).click();

  // Step 2 — credentials, then probe the connection.
  await page.getByLabel("Host").fill("imap.example.com");
  await page.getByLabel("Source user").fill("founder@oldco.com");
  await page.getByLabel("Source password").fill("hunter2");

  await page.getByRole("button", { name: "Test connection" }).click();
  await expect(page.getByText("IMAP login succeeded.")).toBeVisible();

  // Step 3 — confirm + start the import.
  await page.getByRole("button", { name: /Next/ }).click();

  const [createResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/api/v1/migrations") &&
        r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Start migration" }).click(),
  ]);
  expect(createResponse.status()).toBe(201);

  // The wizard reloads the jobs table after the import starts.
  await expect(
    page.getByRole("cell", { name: "founder@oldcompany.com" }),
  ).toBeVisible();
});
