/**
 * Golden path 7 — Signup → create tenant → onboarding checklist.
 *
 * Completes the four-step signup wizard and submits. The mock signup
 * endpoint returns a same-origin "checkout" URL, so the redirect
 * lands back in the SPA's processing screen, which polls the status
 * endpoint (mock reports the tenant active) and routes to the
 * post-signup admin area. From there we open the onboarding checklist
 * the new tenant works through.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

test("sign up, provision the tenant, and reach onboarding", async ({
  page,
}) => {
  await gotoMocked(page, "/signup");

  await expect(
    page.getByRole("heading", { name: "Create your KMail workspace" }),
  ).toBeVisible();

  // Step 0 — work email.
  await page.getByLabel("Work email").fill("founder@newco.com");
  await page.getByRole("button", { name: "Continue" }).click();

  // Step 1 — organization + domain.
  await page.getByLabel("Organization name").fill("NewCo");
  await page.getByLabel(/Email domain/).fill("newco.com");
  await page.getByRole("button", { name: "Continue" }).click();

  // Step 2 — plan selection.
  await page.getByRole("radio", { name: /^Pro/ }).click();
  await page.getByRole("button", { name: "Continue" }).click();

  // Step 3 — review + pay. Submitting initiates the signup.
  const [signupResponse] = await Promise.all([
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/api/v1/signup") &&
        r.request().method() === "POST",
    ),
    page.getByRole("button", { name: "Continue to payment" }).click(),
  ]);
  expect(signupResponse.ok()).toBeTruthy();

  // The same-origin checkout redirect + status poll provision the
  // tenant and route to the post-signup admin destination.
  await page.waitForURL("**/admin/dns-wizard");

  // The new tenant works through the onboarding checklist.
  await gotoMocked(page, "/admin/onboarding");
  await expect(
    page.getByRole("heading", { name: "Onboarding checklist" }),
  ).toBeVisible();
  await selectTenant(page);
  await expect(
    page.getByRole("heading", { name: /Verify your domain/ }),
  ).toBeVisible();
});
