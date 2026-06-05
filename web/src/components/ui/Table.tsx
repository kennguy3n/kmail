import type { ReactNode } from "react";

import styles from "./Table.module.css";

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

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
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
    <div className={styles.wrap}>
      <table className={cx(styles.table, className)}>
        {caption && <caption className={styles.caption}>{caption}</caption>}
        <thead>
          <tr>
            {columns.map((col) => (
              <th
                key={col.key}
                scope="col"
                style={{ width: col.width, textAlign: col.align }}
              >
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className={styles.empty}>
                {emptyContent ?? "No data"}
              </td>
            </tr>
          ) : (
            rows.map((row, rowIndex) => (
              <tr
                key={rowKey(row, rowIndex)}
                className={onRowClick ? styles.clickable : undefined}
                onClick={onRowClick ? () => onRowClick(row, rowIndex) : undefined}
              >
                {columns.map((col) => (
                  <td key={col.key} style={{ textAlign: col.align }}>
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
