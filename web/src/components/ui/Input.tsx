import { forwardRef, useId } from "react";
import type { InputHTMLAttributes, ReactNode } from "react";

import styles from "./Field.module.css";

export interface InputProps
  extends Omit<InputHTMLAttributes<HTMLInputElement>, "size"> {
  /** Visible label. Omit only when an external label/aria-label is set. */
  label?: ReactNode;
  /** Helper text shown beneath the field. */
  hint?: ReactNode;
  /** Error message; sets `aria-invalid` and replaces the hint. */
  error?: ReactNode;
  /** Visually mark the field optional/required without `required` semantics. */
  requiredMark?: boolean;
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Input — a labelled text field wired for accessibility: the label
 * is associated via `htmlFor`, hint/error text is linked through
 * `aria-describedby`, and `aria-invalid` is set when `error` is
 * present.
 */
export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  { label, hint, error, requiredMark, id, className, ...rest },
  ref,
) {
  const generatedId = useId();
  const inputId = id ?? generatedId;
  const describedById = hint || error ? `${inputId}-desc` : undefined;

  return (
    <div className={styles.field}>
      {label && (
        <label htmlFor={inputId} className={styles.label}>
          {label}
          {requiredMark && (
            <span className={styles.required} aria-hidden="true">
              {" "}
              *
            </span>
          )}
        </label>
      )}
      <input
        ref={ref}
        id={inputId}
        className={cx(styles.control, !!error && styles.invalid, className)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedById}
        {...rest}
      />
      {error ? (
        <p id={describedById} className={styles.error}>
          {error}
        </p>
      ) : (
        hint && (
          <p id={describedById} className={styles.hint}>
            {hint}
          </p>
        )
      )}
    </div>
  );
});
