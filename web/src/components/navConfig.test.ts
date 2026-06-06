/**
 * Unit tests for the navigation model helpers.
 *
 * Pins the breadcrumb derivation (including the longest-prefix
 * fallback for dynamic routes) and the auto-expand group resolution
 * that the sidebar relies on.
 */
import { describe, expect, it } from "vitest";

import { breadcrumbsForPath, expandedGroupIdsForPath } from "./navConfig";

describe("breadcrumbsForPath", () => {
  it("builds a trail for a top-level leaf", () => {
    expect(breadcrumbsForPath("/mail")).toEqual([
      { label: "Mail" },
      { label: "Inbox", to: "/mail" },
    ]);
  });

  it("includes sub-group ancestors for nested admin routes", () => {
    expect(breadcrumbsForPath("/admin/dkim")).toEqual([
      { label: "Admin" },
      { label: "Domains" },
      { label: "DKIM keys", to: "/admin/dkim" },
    ]);
  });

  it("falls back to the longest known prefix for dynamic routes", () => {
    // e.g. /mail/:mailboxId/:emailId resolves to the Inbox crumb.
    expect(breadcrumbsForPath("/mail/abc123/msg-9")).toEqual([
      { label: "Mail" },
      { label: "Inbox", to: "/mail" },
    ]);
  });

  it("resolves the Priority Inbox to its own crumb (not Inbox)", () => {
    // Priority is a dedicated `/mail/priority` route so pathname-based
    // breadcrumb matching surfaces it instead of collapsing to Inbox.
    expect(breadcrumbsForPath("/mail/priority")).toEqual([
      { label: "Mail" },
      { label: "Priority", to: "/mail/priority" },
    ]);
  });

  it("returns an empty trail for unknown routes", () => {
    expect(breadcrumbsForPath("/totally-unknown")).toEqual([]);
  });
});

describe("expandedGroupIdsForPath", () => {
  it("returns the ancestor group chain for a nested route", () => {
    const ids = expandedGroupIdsForPath("/admin/cmk");
    expect(ids).toContain("admin");
    expect(ids).toContain("admin-security");
  });

  it("returns the single top-level group for a shallow route", () => {
    expect(expandedGroupIdsForPath("/calendar")).toEqual(["calendar"]);
  });

  it("expands the Mail group for the Priority Inbox route", () => {
    expect(expandedGroupIdsForPath("/mail/priority")).toEqual(["mail"]);
  });
});
