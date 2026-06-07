import { forwardRef } from "react";
import type { ButtonHTMLAttributes, ReactNode } from "react";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "../../lib/cn";

export type ButtonVariant =
  | "primary"
  | "secondary"
  | "ghost"
  | "danger"
  | "link";
export type ButtonSize = "sm" | "md" | "lg";

/**
 * `class-variance-authority` recipe for the button. The base classes
 * cover layout / focus / disabled handling; `variants` map the public
 * `variant` + `size` props onto Tailwind utilities that read the
 * semantic colour tokens (so light/dark theming is automatic).
 */
const button = cva(
  "relative inline-flex select-none items-center justify-center gap-2 whitespace-nowrap rounded-md font-medium leading-none outline-none transition-[background-color,border-color,color,box-shadow] duration-150 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-canvas disabled:cursor-not-allowed disabled:opacity-60",
  {
    variants: {
      variant: {
        primary:
          "bg-primary text-primary-fg shadow-sm hover:bg-primary-hover active:bg-primary-active",
        secondary:
          "border border-border-strong bg-surface text-fg hover:bg-surface-hover hover:border-fg-muted",
        ghost: "bg-transparent text-fg hover:bg-surface-hover",
        danger:
          "bg-danger text-white shadow-sm hover:bg-danger-hover active:bg-danger-hover",
        link: "bg-transparent text-primary underline-offset-4 hover:underline",
      },
      size: {
        sm: "min-h-9 px-3 text-sm",
        md: "min-h-11 px-4 text-sm",
        lg: "min-h-12 px-5 text-base",
      },
      block: { true: "w-full", false: "" },
    },
    compoundVariants: [
      // The link variant is text-like: drop the min touch target and
      // horizontal padding so it sits inline with body copy.
      { variant: "link", size: ["sm", "md", "lg"], class: "min-h-0 px-0" },
    ],
    defaultVariants: { variant: "secondary", size: "md" },
  },
);

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof button> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  /** Stretch to fill the container width. */
  block?: boolean;
  /** Show a spinner and disable interaction. */
  loading?: boolean;
  /** Optional leading icon (decorative). */
  iconLeft?: ReactNode;
  /** Optional trailing icon (decorative). */
  iconRight?: ReactNode;
}

/**
 * Button — the primary interactive control. Renders a real
 * `<button>` (defaulting to `type="button"` so it never
 * accidentally submits a form), tracks busy state with `loading`,
 * and meets the 44px minimum touch target via the `md` size.
 */
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  function Button(
    {
      variant = "secondary",
      size = "md",
      block = false,
      loading = false,
      iconLeft,
      iconRight,
      disabled,
      type,
      className,
      children,
      ...rest
    },
    ref,
  ) {
    return (
      <button
        ref={ref}
        type={type ?? "button"}
        className={cn(button({ variant, size, block }), className)}
        disabled={disabled || loading}
        aria-busy={loading || undefined}
        {...rest}
      >
        {loading && (
          <span
            className="size-4 shrink-0 animate-spin rounded-full border-2 border-current border-r-transparent"
            aria-hidden="true"
          />
        )}
        {!loading && iconLeft && (
          <span className="inline-flex shrink-0 [&>svg]:size-4" aria-hidden="true">
            {iconLeft}
          </span>
        )}
        {children != null && <span className="truncate">{children}</span>}
        {!loading && iconRight && (
          <span className="inline-flex shrink-0 [&>svg]:size-4" aria-hidden="true">
            {iconRight}
          </span>
        )}
      </button>
    );
  },
);
