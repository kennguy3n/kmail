/**
 * Golden path 2 — Compose → send → confirm.
 *
 * Opens the standalone composer, fills a recipient + subject, and
 * sends. The mock `EmailSubmission/set` resolves without an undo
 * hold, so the composer confirms the send by navigating back to the
 * inbox.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked } from "./helpers";

test("compose a new message and send it", async ({ page }) => {
  await gotoMocked(page, "/mail/compose");

  await expect(
    page.getByRole("heading", { name: "New message" }),
  ).toBeVisible();

  // Send stays disabled until an identity + drafts mailbox load and a
  // recipient is present — filling To is what satisfies the last gate.
  await page.getByLabel("To recipients").fill("alice@acme.com");
  await page.getByLabel("Subject").fill("Lunch on Friday?");

  const send = page.getByTestId("compose-send");
  await expect(send).toBeEnabled();
  await send.click();

  // Confirmation: the composer routes back to the inbox after the
  // submission resolves.
  await page.waitForURL("**/mail");
  await expect(
    page.getByRole("button", { name: /Welcome to KMail!/ }),
  ).toBeVisible();
});
