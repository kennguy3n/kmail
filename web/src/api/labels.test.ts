/**
 * Unit tests for the label/tag registry and its keyword mapping.
 */
import { beforeEach, describe, expect, it } from "vitest";

import {
  createLabel,
  deleteLabel,
  isLabelKeyword,
  labelByKeyword,
  labelKeyword,
  labelsForKeywords,
  listLabels,
  updateLabel,
} from "./labels";

beforeEach(() => {
  localStorage.clear();
});

describe("labelKeyword / isLabelKeyword", () => {
  it("slugs the name into a namespaced, unique keyword", () => {
    const kw = labelKeyword("Work / Projects!");
    expect(kw.startsWith("kmlabel_work_projects_")).toBe(true);
    expect(isLabelKeyword(kw)).toBe(true);
  });

  it("does not treat system keywords as labels", () => {
    expect(isLabelKeyword("$seen")).toBe(false);
  });

  it("generates distinct keywords for same-named labels", () => {
    expect(labelKeyword("Work")).not.toBe(labelKeyword("Work"));
  });
});

describe("label CRUD + lookups", () => {
  it("creates, renames (keeping keyword), and deletes", () => {
    const label = createLabel({ name: "Work", color: "#3b82f6" });
    expect(listLabels()).toHaveLength(1);
    expect(labelByKeyword(label.keyword)?.name).toBe("Work");

    const renamed = updateLabel(label.id, { name: "Job", color: "#ef4444" });
    expect(renamed.keyword).toBe(label.keyword);
    expect(renamed.name).toBe("Job");

    deleteLabel(label.id);
    expect(listLabels()).toHaveLength(0);
  });
});

describe("labelsForKeywords", () => {
  it("maps an email's true keywords to their labels", () => {
    const work = createLabel({ name: "Work", color: "#3b82f6" });
    const home = createLabel({ name: "Home", color: "#22c55e" });
    const labels = labelsForKeywords({
      [work.keyword]: true,
      [home.keyword]: false,
      $seen: true,
    });
    expect(labels.map((l) => l.name)).toEqual(["Work"]);
  });
});
