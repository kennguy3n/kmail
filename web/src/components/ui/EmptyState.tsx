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

/** EmptyState — friendly placeholder for empty lists / zero results. */
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
        "flex flex-col items-center justify-center gap-3 px-6 py-12 text-center",
        className,
      )}
    >
      {icon && (
        <div
          className="flex size-12 items-center justify-center rounded-full bg-surface-muted text-fg-subtle [&>svg]:size-6"
          aria-hidden="true"
        >
          {icon}
        </div>
      )}
      <p className="text-base font-semibold text-fg">{title}</p>
      {description && (
        <p className="max-w-prose text-sm text-fg-muted">{description}</p>
      )}
      {action && <div className="mt-1">{action}</div>}
    </div>
  );
}
