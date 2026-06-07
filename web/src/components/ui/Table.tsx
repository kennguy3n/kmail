import type { ReactNode } from "react";

import { cn } from "../../lib/cn";

export interface TableColumn<Row> {
  /** Stable key; also used as React key for header/cells. */
  key: string;
  header: ReactNode;
  /** Cell renderer for a given row. */
  render: (row: Row, rowIndex: number) => ReactNode;
  /** Optional fixed/relative width (CSS value). */
  width?: string;
  /** Text alignment within the column. */
  align?: "left" | "center" | "right";
}

export interface TableProps<Row> {
  columns: TableColumn<Row>[];
  rows: Row[];
  /** Stable row key extractor. */
  rowKey: (row: Row, index: number) => string;
  /** Optional caption for screen readers / context. */
  caption?: ReactNode;
  /** Content shown when `rows` is empty. */
  emptyContent?: ReactNode;
  /** Row click handler (makes rows interactive). */
  onRowClick?: (row: Row, index: number) => void;
  className?: string;
}

/** Table — a themed, generic data table with semantic markup. */
export function Table<Row>({
  columns,
  rows,
  rowKey,
  caption,
  emptyContent,
  onRowClick,
  className,
}: TableProps<Row>): JSX.Element {
  return (
    <div className="w-full overflow-x-auto">
      <table className={cn("w-full border-collapse text-sm", className)}>
        {caption && (
          <caption className="px-3 py-2 text-left text-xs text-fg-muted">
            {caption}
          </caption>
        )}
        <thead>
          <tr className="border-b border-border">
            {columns.map((col) => (
              <th
                key={col.key}
                scope="col"
                className="px-3 py-2.5 text-xs font-semibold uppercase tracking-wide text-fg-muted"
                style={{
                  width: col.width,
                  textAlign: col.align ?? "left",
                }}
              >
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td
                colSpan={columns.length}
                className="px-3 py-10 text-center text-fg-muted"
              >
                {emptyContent ?? "No data"}
              </td>
            </tr>
          ) : (
            rows.map((row, rowIndex) => (
              <tr
                key={rowKey(row, rowIndex)}
                className={cn(
                  "border-b border-border/70 transition-colors last:border-0",
                  onRowClick &&
                    "cursor-pointer hover:bg-surface-hover",
                )}
                onClick={onRowClick ? () => onRowClick(row, rowIndex) : undefined}
              >
                {columns.map((col) => (
                  <td
                    key={col.key}
                    className="px-3 py-2.5 text-fg"
                    style={{ textAlign: col.align ?? "left" }}
                  >
                    {col.render(row, rowIndex)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  );
}
