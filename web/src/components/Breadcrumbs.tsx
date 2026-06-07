import { Fragment } from "react";
import { Link } from "react-router-dom";

import { cn } from "../lib/cn";

export interface Crumb {
  label: string;
  /** Link target; omit for the current (last) crumb. */
  to?: string;
}

export interface BreadcrumbsProps {
  items: Crumb[];
  className?: string;
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
    <nav aria-label="Breadcrumb" className={cn("text-sm", className)}>
      <ol className="flex flex-wrap items-center gap-1.5">
        {items.map((crumb, i) => {
          const isLast = i === items.length - 1;
          return (
            <Fragment key={`${crumb.label}-${i}`}>
              <li className="inline-flex items-center">
                {crumb.to && !isLast ? (
                  <Link
                    to={crumb.to}
                    className="text-fg-muted transition-colors hover:text-fg hover:underline"
                  >
                    {crumb.label}
                  </Link>
                ) : (
                  <span
                    className={cn(isLast ? "font-medium text-fg" : "text-fg-muted")}
                    aria-current={isLast ? "page" : undefined}
                  >
                    {crumb.label}
                  </span>
                )}
              </li>
              {!isLast && (
                <li className="select-none text-fg-subtle" aria-hidden="true">
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
