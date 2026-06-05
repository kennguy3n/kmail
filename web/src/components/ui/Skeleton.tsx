import type { CSSProperties } from "react";

import styles from "./Skeleton.module.css";

export interface SkeletonProps {
  /** CSS width (e.g. "100%", "8rem"). */
  width?: string;
  /** CSS height (e.g. "1rem"). */
  height?: string;
  /** Render as a circle (e.g. avatar placeholder). */
  circle?: boolean;
  /** Number of stacked lines (for multi-line text placeholders). */
  lines?: number;
  className?: string;
  /** Accessible loading label for the wrapping status region. */
  label?: string;
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Skeleton — an animated loading placeholder. A single instance is
 * `aria-hidden`; pass `label` to wrap it in a `role="status"`
 * region so assistive tech announces the loading state once.
 */
export function Skeleton({
  width,
  height,
  circle = false,
  lines = 1,
  className,
  label,
}: SkeletonProps): JSX.Element {
  const style: CSSProperties = {
    width: circle ? height ?? width : width,
    height,
    borderRadius: circle ? "50%" : undefined,
  };

  const content =
    lines > 1 ? (
      <span className={styles.stack}>
        {Array.from({ length: lines }).map((_, i) => (
          <span
            key={i}
            className={cx(styles.skeleton, className)}
            style={{
              ...style,
              // Last line is shorter to mimic real text.
              width: i === lines - 1 ? "60%" : style.width ?? "100%",
            }}
          />
        ))}
      </span>
    ) : (
      <span
        className={cx(styles.skeleton, className)}
        style={style}
        aria-hidden="true"
      />
    );

  if (label) {
    return (
      <span role="status" aria-busy="true" className={styles.status}>
        <span className="visually-hidden">{label}</span>
        {content}
      </span>
    );
  }
  return content;
}
