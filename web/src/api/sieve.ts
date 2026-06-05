/**
 * Vacation / auto-reply Sieve script generator for the
 * Out-of-Office editor.
 *
 * The per-tenant Sieve rule CRUD already lives in `api/admin.ts`
 * (`listSieveRules`, `createSieveRule`, …) — this module only owns
 * the bit that's specific to WS2: turning normalized
 * {@link VacationSettings} into an RFC 5230 `vacation` script, and
 * naming the well-known rule the OOO editor reads/writes.
 */
import type { VacationSettings } from "../types";

/** The reserved rule name the OOO editor reads/writes. */
export const VACATION_RULE_NAME = "Out of Office";

/**
 * Escape a string for inclusion in a Sieve quoted string (RFC 5228
 * §2.4.2): backslash and double-quote are the only characters that
 * must be escaped inside `"..."`.
 */
function sieveQuote(value: string): string {
  return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

/**
 * Generate an RFC 5230 `vacation` Sieve script from normalized
 * {@link VacationSettings}.
 *
 * - The reply subject and body come straight from the settings.
 * - A date range, when supplied, wraps the `vacation` action in a
 *   `currentdate` guard (RFC 5260) so the auto-reply only fires
 *   inside the window. `startDate`/`endDate` are inclusive.
 * - `:days 1` collapses repeat replies to the same sender to once
 *   per day, the conventional vacation cadence.
 *
 * Note: `contactsOnly` cannot be expressed in stock Sieve without a
 * server-specific address-book extension, so it is surfaced to the
 * user as a best-effort flag and recorded as a comment in the
 * script rather than silently ignored.
 */
export function buildVacationScript(settings: VacationSettings): string {
  const requires = new Set<string>(["vacation"]);
  const lines: string[] = [];

  const subject = settings.subject.trim() || "Out of Office";
  const message = settings.message.trim() || "I am currently away.";

  const guards: string[] = [];
  if (settings.startDate) {
    requires.add("date");
    requires.add("relational");
    guards.push(
      `currentdate :value "ge" "date" ${sieveQuote(settings.startDate)}`,
    );
  }
  if (settings.endDate) {
    requires.add("date");
    requires.add("relational");
    guards.push(
      `currentdate :value "le" "date" ${sieveQuote(settings.endDate)}`,
    );
  }

  const requireList = [...requires].map((r) => `"${r}"`).join(", ");
  lines.push(`require [${requireList}];`);
  if (settings.contactsOnly) {
    lines.push(
      "# contactsOnly requested — restrict replies to known senders at the MTA",
    );
  }

  const vacationAction = `vacation :days 1 :subject ${sieveQuote(
    subject,
  )} ${sieveQuote(message)};`;

  if (guards.length > 0) {
    lines.push(`if allof (${guards.join(", ")}) {`);
    lines.push(`  ${vacationAction}`);
    lines.push("}");
  } else {
    lines.push(vacationAction);
  }

  return lines.join("\n") + "\n";
}
