/**
 * Unit tests for the Out-of-Office Sieve script generator.
 *
 * These pin the RFC 5230 `vacation` output the OOO editor deploys:
 * the require set, date guards, quoting/escaping, and the
 * best-effort `contactsOnly` comment.
 */
import { describe, expect, it } from "vitest";

import { buildVacationScript } from "./sieve";
import type { VacationSettings } from "../types";

function settings(overrides: Partial<VacationSettings> = {}): VacationSettings {
  return {
    enabled: true,
    subject: "Away",
    message: "I am out.",
    startDate: null,
    endDate: null,
    contactsOnly: false,
    ...overrides,
  };
}

describe("buildVacationScript", () => {
  it("emits a bare vacation action with no date range", () => {
    const script = buildVacationScript(settings());
    expect(script).toContain('require ["vacation"];');
    expect(script).toContain('vacation :days 1 :subject "Away" "I am out.";');
    expect(script).not.toContain("currentdate");
  });

  it("wraps the action in a currentdate guard when a range is set", () => {
    const script = buildVacationScript(
      settings({ startDate: "2026-01-01", endDate: "2026-01-31" }),
    );
    expect(script).toContain('"date"');
    expect(script).toContain('"relational"');
    expect(script).toContain(
      'currentdate :value "ge" "date" "2026-01-01"',
    );
    expect(script).toContain(
      'currentdate :value "le" "date" "2026-01-31"',
    );
    expect(script).toContain("if allof (");
  });

  it("escapes double quotes in the subject and message", () => {
    const script = buildVacationScript(
      settings({ subject: 'Out "now"', message: 'back "soon"' }),
    );
    expect(script).toContain('\\"now\\"');
    expect(script).toContain('\\"soon\\"');
  });

  it("records contactsOnly as a comment", () => {
    const script = buildVacationScript(settings({ contactsOnly: true }));
    expect(script).toContain("# contactsOnly requested");
  });

  it("falls back to defaults for empty subject/message", () => {
    const script = buildVacationScript(settings({ subject: "  ", message: "" }));
    expect(script).toContain('"Out of Office"');
    expect(script).toContain('"I am currently away."');
  });
});
