/**
 * Golden path 6 — Admin → manage a user → set quota.
 *
 * The SPA's User Admin page manages already-provisioned users
 * (creation happens out-of-band via signup / SCIM), so the
 * user-management golden path is: pick the tenant, edit a user, set a
 * new mailbox quota, and save. The row reflects the new quota after
 * the PATCH resolves.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

// 50 GiB, shown in the table via toLocaleString().
const NEW_QUOTA_BYTES = 50 * 1024 ** 3;
const NEW_QUOTA_LABEL = NEW_QUOTA_BYTES.toLocaleString("en-US");

test("edit a user and set a new quota", async ({ page }) => {
  await gotoMocked(page, "/admin/users");

  await expect(page.getByRole("heading", { name: "User admin" })).toBeVisible();
  await selectTenant(page);

  const aliceRow = page.getByRole("row", { name: /alice@acme\.com/ });
  await expect(aliceRow).toBeVisible();
  await aliceRow.getByRole("button", { name: "Edit" }).click();

  // The editing row exposes a single numeric (quota) input.
  const quotaInput = aliceRow.getByRole("spinbutton");
  await quotaInput.fill(String(NEW_QUOTA_BYTES));

  const [patchResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        /\/users\/user-alice$/.test(r.url()) &&
        r.request().method() === "PATCH",
    ),
    aliceRow.getByRole("button", { name: "Save" }).click(),
  ]);
  expect(patchResponse.ok()).toBeTruthy();

  // Back in display mode, the row shows the updated quota.
  await expect(
    aliceRow.getByRole("cell", { name: NEW_QUOTA_LABEL }),
  ).toBeVisible();
});
