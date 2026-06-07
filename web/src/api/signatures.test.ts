/**
 * Unit tests for the email signature store.
 *
 * The load-bearing invariant is "at most one default per identity
 * scope": creating/updating a default must clear the default flag on
 * any other signature sharing the same `identityEmail` (or the
 * `null` "any identity" scope). `defaultSignatureFor` then resolves
 * the most specific default, falling back to the global one.
 */
import { beforeEach, describe, expect, it } from "vitest";

import {
  createSignature,
  defaultSignatureFor,
  deleteSignature,
  listSignatures,
  updateSignature,
} from "./signatures";

beforeEach(() => {
  localStorage.clear();
});

function draft(overrides = {}) {
  return {
    name: "Work",
    html: "<p>Regards</p>",
    identityEmail: "me@acme.com" as string | null,
    isDefault: false,
    ...overrides,
  };
}

describe("createSignature", () => {
  it("creates a signature with a generated id and timestamps", () => {
    const sig = createSignature(draft());
    expect(sig.id).toBeTruthy();
    expect(sig.createdAt).toBeTruthy();
    expect(listSignatures()).toHaveLength(1);
  });

  it("defaults an empty name to 'Untitled signature'", () => {
    const sig = createSignature(draft({ name: "   " }));
    expect(sig.name).toBe("Untitled signature");
  });

  it("enforces a single default per identity scope", () => {
    const first = createSignature(draft({ name: "A", isDefault: true }));
    const second = createSignature(draft({ name: "B", isDefault: true }));

    const list = listSignatures();
    const a = list.find((s) => s.id === first.id);
    const b = list.find((s) => s.id === second.id);
    expect(b?.isDefault).toBe(true);
    expect(a?.isDefault).toBe(false);
  });

  it("allows independent defaults for different identity scopes", () => {
    createSignature(draft({ identityEmail: "me@acme.com", isDefault: true }));
    createSignature(draft({ identityEmail: null, isDefault: true }));
    const defaults = listSignatures().filter((s) => s.isDefault);
    expect(defaults).toHaveLength(2);
  });
});

describe("updateSignature", () => {
  it("updates fields and re-dedupes defaults", () => {
    const a = createSignature(draft({ name: "A", isDefault: true }));
    const b = createSignature(draft({ name: "B", isDefault: false }));

    updateSignature(b.id, draft({ name: "B", isDefault: true }));
    const list = listSignatures();
    expect(list.find((s) => s.id === a.id)?.isDefault).toBe(false);
    expect(list.find((s) => s.id === b.id)?.isDefault).toBe(true);
  });

  it("throws for an unknown id", () => {
    expect(() => updateSignature("missing", draft())).toThrow(/not found/i);
  });
});

describe("deleteSignature", () => {
  it("removes the signature and is a no-op for unknown ids", () => {
    const sig = createSignature(draft());
    deleteSignature(sig.id);
    expect(listSignatures()).toHaveLength(0);
    expect(() => deleteSignature("missing")).not.toThrow();
  });
});

describe("defaultSignatureFor", () => {
  it("prefers the default scoped to the exact identity (case-insensitive)", () => {
    const scoped = createSignature(
      draft({ name: "scoped", identityEmail: "me@acme.com", isDefault: true }),
    );
    createSignature(
      draft({ name: "global", identityEmail: null, isDefault: true }),
    );
    expect(defaultSignatureFor("ME@ACME.COM")?.id).toBe(scoped.id);
  });

  it("falls back to the global default when no scoped default exists", () => {
    const global = createSignature(
      draft({ name: "global", identityEmail: null, isDefault: true }),
    );
    expect(defaultSignatureFor("someone@else.com")?.id).toBe(global.id);
  });

  it("returns null when there is no applicable default", () => {
    createSignature(draft({ isDefault: false }));
    expect(defaultSignatureFor("me@acme.com")).toBeNull();
  });
});
