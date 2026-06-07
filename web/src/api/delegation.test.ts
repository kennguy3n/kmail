/**
 * Unit tests for the delegate-access / send-as grant store.
 *
 * Delegation is persisted client-side via the localStore seam, so
 * these tests exercise the validation rules (no self-delegation, no
 * duplicate owner→delegate pairs, both emails required), the
 * newest-first ordering, update/delete, and the case-insensitive
 * `sendAsOwnersFor` lookup that Compose uses to surface delegated
 * identities.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  createGrant,
  deleteGrant,
  listGrants,
  sendAsOwnersFor,
  updateGrant,
} from "./delegation";

beforeEach(() => {
  localStorage.clear();
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-01-01T00:00:00.000Z"));
});

afterEach(() => {
  vi.useRealTimers();
});

function draft(overrides = {}) {
  return {
    ownerEmail: "owner@acme.com",
    delegateEmail: "assistant@acme.com",
    access: "read-write" as const,
    sendAs: true,
    ...overrides,
  };
}

describe("createGrant", () => {
  it("creates a grant and persists it", () => {
    const grant = createGrant(draft());
    expect(grant.id).toBeTruthy();
    expect(grant.ownerEmail).toBe("owner@acme.com");
    expect(listGrants()).toHaveLength(1);
  });

  it("rejects self-delegation (case/whitespace-insensitive)", () => {
    expect(() =>
      createGrant(draft({ ownerEmail: "  Boss@Acme.com ", delegateEmail: "boss@acme.com" })),
    ).toThrow(/cannot delegate access to themselves/i);
  });

  it("requires both owner and delegate emails", () => {
    expect(() => createGrant(draft({ ownerEmail: "   " }))).toThrow(
      /both owner and delegate/i,
    );
  });

  it("rejects a duplicate owner→delegate pair", () => {
    createGrant(draft());
    expect(() => createGrant(draft())).toThrow(/already has a grant/i);
  });
});

describe("listGrants", () => {
  it("returns grants newest-first", () => {
    createGrant(draft({ delegateEmail: "first@acme.com" }));
    vi.setSystemTime(new Date("2026-01-02T00:00:00.000Z"));
    createGrant(draft({ delegateEmail: "second@acme.com" }));

    const grants = listGrants();
    expect(grants.map((g) => g.delegateEmail)).toEqual([
      "second@acme.com",
      "first@acme.com",
    ]);
  });
});

describe("updateGrant", () => {
  it("updates access level and send-as flag", () => {
    const grant = createGrant(draft({ access: "read", sendAs: false }));
    const updated = updateGrant(grant.id, { access: "read-write", sendAs: true });
    expect(updated.access).toBe("read-write");
    expect(updated.sendAs).toBe(true);
    expect(listGrants()[0].access).toBe("read-write");
  });

  it("throws for an unknown id", () => {
    expect(() => updateGrant("nope", { access: "read", sendAs: false })).toThrow(
      /not found/i,
    );
  });
});

describe("deleteGrant", () => {
  it("removes the grant", () => {
    const grant = createGrant(draft());
    deleteGrant(grant.id);
    expect(listGrants()).toHaveLength(0);
  });
});

describe("sendAsOwnersFor", () => {
  it("returns owners that granted send-as to the delegate (case-insensitive)", () => {
    createGrant(
      draft({ ownerEmail: "ceo@acme.com", delegateEmail: "ea@acme.com", sendAs: true }),
    );
    createGrant(
      draft({ ownerEmail: "cfo@acme.com", delegateEmail: "ea@acme.com", sendAs: false }),
    );

    expect(sendAsOwnersFor("EA@ACME.COM")).toEqual(["ceo@acme.com"]);
  });
});
