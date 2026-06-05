/**
 * Unit tests for the email-template store and its variable engine.
 *
 * These pin the behaviour the Compose template picker relies on:
 *   1. `renderTemplate` expands `{{var}}` placeholders, tolerates
 *      inner whitespace, and leaves unknown placeholders visible.
 *   2. `extractVariables` returns distinct names in first-seen order.
 *   3. CRUD round-trips through localStorage.
 */
import { beforeEach, describe, expect, it } from "vitest";

import {
  builtinVariables,
  createTemplate,
  deleteTemplate,
  extractVariables,
  listTemplates,
  renderTemplate,
  updateTemplate,
} from "./templates";

beforeEach(() => {
  localStorage.clear();
});

describe("renderTemplate", () => {
  it("expands known placeholders and tolerates inner whitespace", () => {
    const out = renderTemplate("Hi {{ name }}, from {{company}}", {
      name: "Sam",
      company: "Acme",
    });
    expect(out).toBe("Hi Sam, from Acme");
  });

  it("leaves placeholders with no matching value unchanged", () => {
    expect(renderTemplate("Hi {{name}} {{missing}}", { name: "Sam" })).toBe(
      "Hi Sam {{missing}}",
    );
  });
});

describe("extractVariables", () => {
  it("returns distinct names in first-seen order across sources", () => {
    expect(
      extractVariables("{{a}} {{b}}", "{{b}} {{c}} {{a}}"),
    ).toEqual(["a", "b", "c"]);
  });
});

describe("builtinVariables", () => {
  it("supplies date and lets overrides win", () => {
    const vars = builtinVariables({ sender_name: "Sam" });
    expect(vars.sender_name).toBe("Sam");
    expect(typeof vars.date).toBe("string");
    expect(vars.date.length).toBeGreaterThan(0);
  });
});

describe("template CRUD", () => {
  it("creates, updates and deletes a template", () => {
    const created = createTemplate({
      name: "Welcome",
      subject: "Hello {{name}}",
      body: "<p>Hi {{name}}</p>",
      scope: "personal",
    });
    expect(listTemplates()).toHaveLength(1);

    updateTemplate(created.id, {
      name: "Welcome v2",
      subject: created.subject,
      body: created.body,
      scope: "personal",
    });
    expect(listTemplates()[0].name).toBe("Welcome v2");

    deleteTemplate(created.id);
    expect(listTemplates()).toHaveLength(0);
  });
});
