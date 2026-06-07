import { forwardRef, useId } from "react";
import type { InputHTMLAttributes, ReactNode } from "react";

import { cn } from "../../lib/cn";

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

/** Shared control classes for text-like form fields (Input + Select). */
export const fieldControl =
  "w-full min-h-11 rounded-md border border-border-strong bg-surface px-3 py-2 text-sm text-fg transition-[border-color,box-shadow] placeholder:text-fg-subtle enabled:hover:border-fg-muted focus-visible:border-primary focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-primary-subtle disabled:cursor-not-allowed disabled:bg-surface-muted disabled:opacity-70";

export const fieldControlInvalid =
  "border-danger hover:border-danger focus-visible:border-danger focus-visible:ring-danger-bg";

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
    <div className="flex flex-col gap-1">
      {label && (
        <label
          htmlFor={inputId}
          className="text-sm font-medium text-fg"
        >
          {label}
          {requiredMark && (
            <span className="text-danger" aria-hidden="true">
              {" "}
              *
            </span>
          )}
        </label>
      )}
      <input
        ref={ref}
        id={inputId}
        className={cn(fieldControl, !!error && fieldControlInvalid, className)}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedById}
        {...rest}
      />
      {error ? (
        <p id={describedById} className="text-xs text-danger-fg">
          {error}
        </p>
      ) : (
        hint && (
          <p id={describedById} className="text-xs text-fg-muted">
            {hint}
          </p>
        )
      )}
    </div>
  );
});
