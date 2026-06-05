import { forwardRef, useId } from "react";
import type { ReactNode, SelectHTMLAttributes } from "react";

import styles from "./Field.module.css";

export interface SelectOption {
  value: string;
  label: string;
  disabled?: boolean;
}

export interface SelectProps
  extends Omit<SelectHTMLAttributes<HTMLSelectElement>, "size"> {
  label?: ReactNode;
  hint?: ReactNode;
  error?: ReactNode;
  requiredMark?: boolean;
  /** Convenience: render options from data. `children` still works. */
  options?: SelectOption[];
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Select — a labelled native `<select>`. We keep the native control
 * (rather than a custom listbox) for built-in keyboard handling,
 * mobile pickers, and screen-reader support; only the chevron is
 * custom-styled.
 */
export const Select = forwardRef<HTMLSelectElement, SelectProps>(
  function Select(
    { label, hint, error, requiredMark, options, id, className, children, ...rest },
    ref,
  ) {
    const generatedId = useId();
    const selectId = id ?? generatedId;
    const describedById = hint || error ? `${selectId}-desc` : undefined;

    return (
      <div className={styles.field}>
        {label && (
          <label htmlFor={selectId} className={styles.label}>
            {label}
            {requiredMark && (
              <span className={styles.required} aria-hidden="true">
                {" "}
                *
              </span>
            )}
          </label>
        )}
        <div className={styles.selectWrap}>
          <select
            ref={ref}
            id={selectId}
            className={cx(
              styles.control,
              styles.select,
              !!error && styles.invalid,
              className,
            )}
            aria-invalid={error ? true : undefined}
            aria-describedby={describedById}
            {...rest}
          >
            {options
              ? options.map((opt) => (
                  <option
                    key={opt.value}
                    value={opt.value}
                    disabled={opt.disabled}
                  >
                    {opt.label}
                  </option>
                ))
              : children}
          </select>
          <span className={styles.chevron} aria-hidden="true">
            ▾
          </span>
        </div>
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
  },
);
