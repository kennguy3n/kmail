import type { ReactNode } from "react";

import { cn } from "../../lib/cn";

export interface EmptyStateProps {
  /** Decorative icon / illustration. */
  icon?: ReactNode;
  title: ReactNode;
  description?: ReactNode;
  /** Optional call-to-action (e.g. a Button). */
  action?: ReactNode;
  className?: string;
}

/** EmptyState — friendly placeholder for empty lists / zero results.
 *  Aligned to the KChat umbrella: soft surface, generous spacing, indigo
 *  accent icon when no custom icon is provided.
 */
export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: EmptyStateProps): JSX.Element {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center gap-3 rounded-2xl border border-border bg-surface px-6 py-14 text-center shadow-sm",
        className,
      )}
    >
      {icon && (
        <div
          className="mb-1 flex size-14 items-center justify-center rounded-full bg-primary-subtle text-primary [&>svg]:size-7"
          aria-hidden="true"
        >
          {icon}
        </div>
      )}
      <p className="text-base font-semibold text-fg">{title}</p>
      {description && (
        <p className="max-w-prose text-sm leading-relaxed text-fg-muted">
          {description}
        </p>
      )}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}
