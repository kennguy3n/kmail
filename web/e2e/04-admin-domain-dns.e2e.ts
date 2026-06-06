/**
 * Golden path 4 — Admin → DNS wizard → verify a domain.
 *
 * Picks the tenant + primary domain, lets the wizard load every
 * record step, then runs "Verify all records". The verify control
 * round-trips to the mocked verify endpoint and the wizard reports
 * the domain fully verified.
 *
 * The wizard's status loader (`getDnsWizardStatus`) always re-runs
 * verification when a domain is selected, so the verified summary is
 * the deterministic end state for a fully-propagated domain; we still
 * exercise the explicit Verify control and assert its network call
 * fires.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked, selectTenant } from "./helpers";

test("verify a domain through the DNS wizard", async ({ page }) => {
  await gotoMocked(page, "/admin/dns-wizard");

  await expect(
    page.getByRole("heading", { name: /DNS Wizard/i }),
  ).toBeVisible();

  await selectTenant(page);

  const domainSelect = page.getByRole("combobox", { name: /domain/i });
  await expect(
    domainSelect.getByRole("option", { name: "acme.com" }),
  ).toBeAttached();
  await domainSelect.selectOption({ label: "acme.com" });

  // The wizard renders the per-record steps once status loads.
  await expect(
    page.getByRole("heading", { name: /Step 1 \/ \d+: MX records/ }),
  ).toBeVisible();

  // Run the explicit Verify control and confirm it hits the verify
  // endpoint, then assert the verified summary.
  const verifyButton = page.getByRole("button", { name: /Verify all records/ });
  const [verifyResponse] = await Promise.all([
    page.waitForResponse(
      (r) => /\/domains\/[^/]+\/verify$/.test(r.url()) && r.request().method() === "POST",
    ),
    verifyButton.click(),
  ]);
  expect(verifyResponse.ok()).toBeTruthy();

  await expect(
    page.getByRole("heading", { name: "All records verified" }),
  ).toBeVisible();
});
