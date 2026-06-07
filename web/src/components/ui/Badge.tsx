import type { HTMLAttributes, ReactNode } from "react";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "../../lib/cn";

export type BadgeVariant =
  | "neutral"
  | "primary"
  | "success"
  | "warning"
  | "danger"
  | "info";

const badge = cva(
  "inline-flex items-center gap-1.5 rounded-pill border px-2.5 py-0.5 text-xs font-medium leading-tight",
  {
    variants: {
      variant: {
        neutral: "border-border bg-surface-muted text-fg-muted",
        primary: "border-transparent bg-primary-subtle text-on-accent",
        success: "border-transparent bg-success-bg text-success-fg",
        warning: "border-transparent bg-warning-bg text-warning-fg",
        danger: "border-transparent bg-danger-bg text-danger-fg",
        info: "border-transparent bg-info-bg text-info-fg",
      },
    },
    defaultVariants: { variant: "neutral" },
  },
);

const dotColor: Record<BadgeVariant, string> = {
  neutral: "bg-fg-subtle",
  primary: "bg-primary",
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-danger",
  info: "bg-info",
};

export interface BadgeProps
  extends HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badge> {
  variant?: BadgeVariant;
  /** Render a leading status dot before the label. */
  dot?: boolean;
  children: ReactNode;
}

/** Badge — a compact status / count label. */
export function Badge({
  variant = "neutral",
  dot = false,
  className,
  children,
  ...rest
}: BadgeProps): JSX.Element {
  return (
    <span className={cn(badge({ variant }), className)} {...rest}>
      {dot && (
        <span
          className={cn("size-1.5 rounded-full", dotColor[variant])}
          aria-hidden="true"
        />
      )}
      {children}
    </span>
  );
}
