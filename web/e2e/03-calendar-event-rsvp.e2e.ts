/**
 * Golden path 3 — Calendar → create an event → RSVP to an event.
 *
 * Part A drives the event-create form (the calendar auto-selects, so
 * a title is enough to enable Create) and confirms it routes back to
 * the calendar. Part B opens a seeded event and accepts the invite;
 * the RSVP is a `CalendarEvent/set` round-trip whose success is the
 * mocked 200 response plus the absence of an error banner.
 */
import { test, expect } from "@playwright/test";

import { gotoMocked } from "./helpers";

test("create an event then RSVP to a seeded event", async ({ page }) => {
  // The mock seeds calendar events relative to a fixed "now"
  // (2026-04-28); pin the browser clock to that day so the calendar's
  // default week/month window contains the seeded "Team standup".
  await page.clock.setFixedTime(new Date("2026-04-28T12:00:00.000Z"));

  // --- Part A: create an event -----------------------------------
  await gotoMocked(page, "/calendar/new");

  await expect(
    page.getByRole("heading", { name: /new event/i }),
  ).toBeVisible();

  await page.getByLabel("Title").fill("Sprint planning");

  // The calendar picker auto-selects the default calendar once the
  // calendar list loads, which (with the prefilled start/end) is what
  // enables Create event.
  const create = page.getByRole("button", { name: "Create event" });
  await expect(create).toBeEnabled();
  await create.click();

  // On success the form routes back to the calendar grid.
  await page.waitForURL("**/calendar");
  await expect(page.getByRole("heading", { name: "Calendar" })).toBeVisible();

  // --- Part B: RSVP to an existing event -------------------------
  // Month view guarantees the seeded "Team standup" (today) is shown.
  await page.getByRole("tab", { name: "Month" }).click();

  const chip = page.getByRole("button", { name: /Team standup/ }).first();
  await expect(chip).toBeVisible();
  await chip.click();

  // The details panel exposes the RSVP controls.
  const details = page.getByRole("complementary", { name: "Event details" });
  await expect(
    details.getByRole("heading", { name: "Team standup" }),
  ).toBeVisible();

  // Accepting fires a CalendarEvent/set; wait on the mocked JMAP
  // response so the assertion is deterministic, not timed.
  const [rsvpResponse] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes("/jmap") && r.request().method() === "POST",
    ),
    details.getByRole("button", { name: "Accept" }).click(),
  ]);
  expect(rsvpResponse.ok()).toBeTruthy();

  // A failed RSVP would surface an error alert; there should be none.
  await expect(page.getByRole("alert")).toHaveCount(0);
});
