/**
 * Golden path 1 — Inbox → read an email → reply.
 *
 * The SPA has no auth wall in front of the mock layer (the BFF owns
 * sessions), so "login" here is landing on the authenticated inbox at
 * `/mail`. We open the seeded "Welcome to KMail!" message, hit Reply,
 * and confirm the composer opens pre-addressed and sends — landing
 * back on the inbox, which is the product's send-confirmation.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked } from "./helpers";

test("inbox → open message → reply → send returns to inbox", async ({
  page,
}) => {
  await gotoMocked(page, "/mail");

  // The inbox renders one accessible button per message; its name is
  // built from sender + subject + date. Open the welcome message.
  const welcomeRow = page.getByRole("button", { name: /Welcome to KMail!/ });
  await expect(welcomeRow).toBeVisible();
  await welcomeRow.click();

  // MessageView shows the subject as the page heading.
  await page.waitForURL(/\/mail\/[^/]+\/msg-1$/);
  await expect(
    page.getByRole("heading", { name: "Welcome to KMail!" }),
  ).toBeVisible();

  // Reply opens the composer pre-addressed to the original sender.
  await page.getByRole("button", { name: "Reply", exact: true }).click();
  await page.waitForURL("**/mail/compose");

  // Subject is prefilled with the Re: prefix and the recipient is
  // seeded, which is what flips the Send button to enabled.
  await expect(page.getByLabel("Subject")).toHaveValue(/^Re: Welcome to KMail!/);
  const send = page.getByTestId("compose-send");
  await expect(send).toBeEnabled();

  await send.click();

  // Immediate (non-undo) sends confirm by routing back to the inbox.
  await page.waitForURL("**/mail");
  await expect(
    page.getByRole("button", { name: /Welcome to KMail!/ }),
  ).toBeVisible();
});
