/**
 * Unit tests for the typed localStorage wrapper.
 *
 * The store is the persistence seam for the WS2 client-side feature
 * set (signatures, templates, delegation, label colours), so its
 * defensive contract matters: namespaced keys, fallbacks for
 * missing/corrupt values, success/failure signalling on write, and
 * a unique-id generator that degrades gracefully.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { newId, readJSON, removeKey, writeJSON } from "./localStore";

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("readJSON / writeJSON", () => {
  it("round-trips a value under the kmail. namespace", () => {
    expect(writeJSON("greeting", { hi: "there" })).toBe(true);
    // Persisted under the namespaced key, not the bare key.
    expect(localStorage.getItem("kmail.greeting")).toBe(
      JSON.stringify({ hi: "there" }),
    );
    expect(localStorage.getItem("greeting")).toBeNull();
    expect(readJSON("greeting", null)).toEqual({ hi: "there" });
  });

  it("returns the fallback for a missing key", () => {
    expect(readJSON("absent", "default")).toBe("default");
    expect(readJSON<number[]>("absent", [])).toEqual([]);
  });

  it("returns the fallback (not throw) for a corrupt JSON value", () => {
    localStorage.setItem("kmail.bad", "{not valid json");
    expect(readJSON("bad", "fallback")).toBe("fallback");
  });

  it("signals a failed write when serialization throws", () => {
    const circular: Record<string, unknown> = {};
    circular.self = circular;
    expect(writeJSON("cycle", circular)).toBe(false);
  });
});

describe("removeKey", () => {
  it("removes a namespaced value", () => {
    writeJSON("temp", 1);
    removeKey("temp");
    expect(readJSON("temp", "gone")).toBe("gone");
  });
});

describe("newId", () => {
  it("generates unique, non-empty ids", () => {
    const a = newId();
    const b = newId();
    expect(a).toBeTruthy();
    expect(a).not.toBe(b);
  });

  it("falls back to a timestamp+random id when crypto.randomUUID is unavailable", () => {
    vi.spyOn(crypto, "randomUUID").mockImplementation(() => {
      throw new Error("unavailable");
    });
    const id = newId();
    expect(id).toMatch(/^id-/);
  });
});
