/**
 * Golden path 8 — Shared inbox → assign → add note → resolve.
 *
 * Loads the shared-inbox workflow overlay for a support inbox, picks
 * the open assignment, assigns it to a teammate, adds an internal
 * note, and moves it to "resolved". Each mutation is awaited via its
 * network call and confirmed against the UI it drives.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked } from "./helpers";

test("assign an email, add a note, then resolve it", async ({ page }) => {
  await gotoMocked(page, "/mail/shared");
  await expect(
    page.getByRole("heading", { name: "Shared inbox workflows" }),
  ).toBeVisible();

  // Loading an inbox id fetches its assignments.
  await page.getByLabel("Shared inbox ID").fill("shared-support");

  const row = page.getByRole("row", { name: /msg-2/ });
  await expect(row).toBeVisible();
  await row.click();

  // Detail panel opens for the selected assignment.
  await expect(page.getByRole("heading", { name: "msg-2" })).toBeVisible();

  // Assign to a teammate.
  await page.getByLabel("Assign to").fill("user-alice");
  const [assignResponse] = await Promise.all([
    page.waitForResponse(
      (r) => /\/emails\/msg-2\/assign$/.test(r.url()) && r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Assign" }).click(),
  ]);
  expect(assignResponse.ok()).toBeTruthy();

  // Add an internal note; it appears in the notes list afterwards.
  await page.getByLabel("Internal note").fill("Following up with billing.");
  const [noteResponse] = await Promise.all([
    page.waitForResponse(
      (r) => /\/emails\/msg-2\/notes$/.test(r.url()) && r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Add note" }).click(),
  ]);
  expect(noteResponse.status()).toBe(201);
  await expect(page.getByText("Following up with billing.")).toBeVisible();

  // Resolve the assignment via the detail status selector. The list
  // filter and the detail panel both render a control labelled "Status",
  // so target the detail one by its distinct accessible name rather than
  // a positional `.last()` that breaks if the DOM gains another combobox.
  const statusSelect = page.getByLabel("Assignment status");
  const [statusResponse] = await Promise.all([
    page.waitForResponse(
      (r) => /\/emails\/msg-2\/status$/.test(r.url()) && r.request().method() === "PUT",
    ),
    statusSelect.selectOption("resolved"),
  ]);
  expect(statusResponse.ok()).toBeTruthy();
  await expect(statusSelect).toHaveValue("resolved");
});
