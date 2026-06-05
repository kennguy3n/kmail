import { forwardRef } from "react";
import type { ButtonHTMLAttributes, ReactNode } from "react";

import styles from "./Button.module.css";

export type ButtonVariant =
  | "primary"
  | "secondary"
  | "ghost"
  | "danger"
  | "link";
export type ButtonSize = "sm" | "md" | "lg";

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
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

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Button — the primary interactive control. Renders a real
 * `<button>` (defaulting to `type="button"` so it never
 * accidentally submits a form), tracks busy state with `loading`,
 * and meets the 44px minimum touch target via the design tokens.
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
        className={cx(
          styles.button,
          styles[variant],
          styles[size],
          block && styles.block,
          loading && styles.loading,
          className,
        )}
        disabled={disabled || loading}
        aria-busy={loading || undefined}
        {...rest}
      >
        {loading && <span className={styles.spinner} aria-hidden="true" />}
        {!loading && iconLeft && (
          <span className={styles.icon} aria-hidden="true">
            {iconLeft}
          </span>
        )}
        {children != null && <span className={styles.label}>{children}</span>}
        {!loading && iconRight && (
          <span className={styles.icon} aria-hidden="true">
            {iconRight}
          </span>
        )}
      </button>
    );
  },
);
