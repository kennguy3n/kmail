import { Fragment } from "react";
import { Link } from "react-router-dom";

import styles from "./Breadcrumbs.module.css";

export interface Crumb {
  label: string;
  /** Link target; omit for the current (last) crumb. */
  to?: string;
}

export interface BreadcrumbsProps {
  items: Crumb[];
  className?: string;
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Breadcrumbs — a navigational trail. The wrapping `<nav>` is
 * labelled "Breadcrumb" and the current page is marked with
 * `aria-current="page"` per the WAI-ARIA breadcrumb pattern.
 */
export function Breadcrumbs({
  items,
  className,
}: BreadcrumbsProps): JSX.Element | null {
  if (items.length === 0) return null;
  return (
    <nav aria-label="Breadcrumb" className={cx(styles.breadcrumbs, className)}>
      <ol className={styles.list}>
        {items.map((crumb, i) => {
          const isLast = i === items.length - 1;
          return (
            <Fragment key={`${crumb.label}-${i}`}>
              <li className={styles.item}>
                {crumb.to && !isLast ? (
                  <Link to={crumb.to} className={styles.link}>
                    {crumb.label}
                  </Link>
                ) : (
                  <span
                    className={styles.current}
                    aria-current={isLast ? "page" : undefined}
                  >
                    {crumb.label}
                  </span>
                )}
              </li>
              {!isLast && (
                <li className={styles.sep} aria-hidden="true">
                  /
                </li>
              )}
            </Fragment>
          );
        })}
      </ol>
    </nav>
  );
}
