/**
 * Unit tests for the generic Table primitive.
 *
 * Covers semantic markup (column headers with scope, a caption),
 * row rendering through the `render` cell callback, the empty state,
 * and interactive rows (onRowClick). Queries go through table roles
 * (`columnheader`, `row`, `cell`) so they don't depend on styling.
 */
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { Table } from "./Table";
import type { TableColumn } from "./Table";

interface Person {
  id: string;
  name: string;
  age: number;
}

const people: Person[] = [
  { id: "p1", name: "Ada", age: 36 },
  { id: "p2", name: "Alan", age: 41 },
];

const columns: TableColumn<Person>[] = [
  { key: "name", header: "Name", render: (r) => r.name },
  { key: "age", header: "Age", align: "right", render: (r) => r.age },
];

describe("Table", () => {
  it("renders column headers as scoped <th> cells", () => {
    render(
      <Table columns={columns} rows={people} rowKey={(r) => r.id} />,
    );
    const headers = screen.getAllByRole("columnheader");
    expect(headers.map((h) => h.textContent)).toEqual(["Name", "Age"]);
    expect(headers[0]).toHaveAttribute("scope", "col");
  });

  it("renders a row per datum via the cell render callback", () => {
    render(
      <Table columns={columns} rows={people} rowKey={(r) => r.id} />,
    );
    expect(screen.getByRole("cell", { name: "Ada" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "Alan" })).toBeInTheDocument();
    // Header row + 2 body rows.
    expect(screen.getAllByRole("row")).toHaveLength(3);
  });

  it("associates a caption with the table for screen readers", () => {
    render(
      <Table
        columns={columns}
        rows={people}
        rowKey={(r) => r.id}
        caption="Team roster"
      />,
    );
    expect(screen.getByRole("table", { name: "Team roster" })).toBeInTheDocument();
  });

  it("shows the empty content when there are no rows", () => {
    render(
      <Table
        columns={columns}
        rows={[]}
        rowKey={(r) => r.id}
        emptyContent="Nobody here"
      />,
    );
    expect(screen.getByText("Nobody here")).toBeInTheDocument();
  });

  it("falls back to a default empty message", () => {
    render(<Table columns={columns} rows={[]} rowKey={(r) => r.id} />);
    expect(screen.getByText("No data")).toBeInTheDocument();
  });

  it("invokes onRowClick with the row and index", async () => {
    const onRowClick = vi.fn();
    render(
      <Table
        columns={columns}
        rows={people}
        rowKey={(r) => r.id}
        onRowClick={onRowClick}
      />,
    );
    await userEvent.click(screen.getByRole("cell", { name: "Alan" }));
    expect(onRowClick).toHaveBeenCalledTimes(1);
    expect(onRowClick).toHaveBeenCalledWith(people[1], 1);
  });
});
