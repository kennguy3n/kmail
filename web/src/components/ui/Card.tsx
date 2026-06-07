import type { HTMLAttributes, ReactNode } from "react";

import { cn } from "../../lib/cn";

export interface CardProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "title"> {
  /** Optional header content rendered above a divider. */
  title?: ReactNode;
  /** Optional actions shown on the right of the header. */
  actions?: ReactNode;
  /** Remove body padding (for tables / media that bleed to the edge). */
  flush?: boolean;
  children: ReactNode;
}

/** Card — a surface container with an optional titled header. */
export function Card({
  title,
  actions,
  flush = false,
  className,
  children,
  ...rest
}: CardProps): JSX.Element {
  return (
    <div
      className={cn(
        "overflow-hidden rounded-xl border border-border bg-surface shadow-sm",
        className,
      )}
      {...rest}
    >
      {(title || actions) && (
        <div className="flex items-center justify-between gap-3 border-b border-border px-5 py-3.5">
          {title && (
            <h3 className="text-base font-semibold text-fg">{title}</h3>
          )}
          {actions && (
            <div className="flex items-center gap-2">{actions}</div>
          )}
        </div>
      )}
      <div className={cn(!flush && "p-5")}>{children}</div>
    </div>
  );
}
