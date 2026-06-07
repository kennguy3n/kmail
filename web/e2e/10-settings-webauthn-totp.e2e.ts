/**
 * Golden path 10 — Settings → add a WebAuthn key → add TOTP.
 *
 * On the Security settings page: kick off a WebAuthn (FIDO2)
 * registration challenge, then switch to the TOTP tab and run the full
 * enrol → verify flow, ending with the recovery codes the user must
 * save. The native security-key ceremony itself runs against
 * `navigator.credentials`; this page is the management surface, so the
 * WebAuthn step asserts the issued challenge.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

test("register a security key and enrol TOTP", async ({ page }) => {
  await gotoMocked(page, "/admin/security");
  await expect(page.getByRole("heading", { name: "Security" })).toBeVisible();
  await selectTenant(page);

  // WebAuthn: request a registration challenge.
  const [beginResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/auth/webauthn/register/begin") &&
        r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Register a new key" }).click(),
  ]);
  expect(beginResponse.ok()).toBeTruthy();
  await expect(page.getByText(/Registration challenge issued/)).toBeVisible();

  // Switch to the TOTP tab and begin enrolment.
  await page.getByRole("button", { name: "TOTP (authenticator app)" }).click();
  const [enrollResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/auth/totp/enroll") &&
        r.request().method() === "POST",
    ),
    page.getByRole("button", { name: /enrolment/i }).click(),
  ]);
  expect(enrollResponse.ok()).toBeTruthy();

  // The enrolment secret is shown for manual entry.
  await expect(
    page.getByText("JBSWY3DPEHPK3PXP", { exact: true }),
  ).toBeVisible();

  // Enter a 6-digit code and verify to enable TOTP.
  await page.getByLabel(/6-digit code/).fill("123456");
  const [verifyResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/auth/totp/verify") &&
        r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Verify and enable" }).click(),
  ]);
  expect(verifyResponse.ok()).toBeTruthy();

  // Recovery codes are revealed after enabling.
  await expect(
    page.getByRole("heading", { name: "Recovery codes" }),
  ).toBeVisible();
  await expect(page.getByText("aaaa-bbbb-cccc")).toBeVisible();
});
