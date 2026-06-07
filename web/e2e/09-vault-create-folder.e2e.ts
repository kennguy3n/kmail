/**
 * Golden path 9 — Vault → create folder → set encryption.
 *
 * Creates a Zero-Access (StrictZK) vault folder for the tenant. The
 * encryption mode is intrinsic to a vault folder, so "set encryption"
 * is exercised by acknowledging the zero-access warning, creating the
 * folder, then opening it to confirm its StrictZK encryption metadata.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

const FOLDER_NAME = "Board Minutes";

test("create a StrictZK vault folder and inspect its encryption", async ({
  page,
}) => {
  await gotoMocked(page, "/mail/vault");
  await expect(
    page.getByRole("heading", { name: "Zero-Access Vault" }),
  ).toBeVisible();

  await selectTenant(page);

  // Fill the create form and acknowledge the no-search warning.
  await page.getByLabel("Folder name").fill(FOLDER_NAME);
  await page.getByRole("checkbox").check();

  const createButton = page.getByRole("button", {
    name: "Create vault folder",
  });
  await expect(createButton).toBeEnabled();

  const [createResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        /\/vault\/folders$/.test(r.url()) && r.request().method() === "POST",
    ),
    createButton.click(),
  ]);
  expect(createResponse.status()).toBe(201);

  // The reloaded folder list contains the new folder; open it.
  const folderButton = page
    .getByRole("button", { name: new RegExp(FOLDER_NAME) })
    .first();
  await expect(folderButton).toBeVisible();
  await folderButton.click();

  // The detail panel shows the StrictZK encryption metadata.
  const detail = page.getByRole("article");
  await expect(
    detail.getByRole("heading", { name: new RegExp(FOLDER_NAME) }),
  ).toBeVisible();
  await expect(detail.getByText("Encryption mode")).toBeVisible();
  await expect(detail.getByText("StrictZK")).toBeVisible();
});
