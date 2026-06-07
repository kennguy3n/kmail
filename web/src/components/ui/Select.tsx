import { forwardRef, useId } from "react";
import type { ReactNode, SelectHTMLAttributes } from "react";
import { ChevronDown } from "lucide-react";

import { cn } from "../../lib/cn";
import { fieldControl, fieldControlInvalid } from "./Input";

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
      <div className="flex flex-col gap-1">
        {label && (
          <label htmlFor={selectId} className="text-sm font-medium text-fg">
            {label}
            {requiredMark && (
              <span className="text-danger" aria-hidden="true">
                {" "}
                *
              </span>
            )}
          </label>
        )}
        <div className="relative flex">
          <select
            ref={ref}
            id={selectId}
            className={cn(
              fieldControl,
              "cursor-pointer appearance-none pr-9",
              !!error && fieldControlInvalid,
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
          <ChevronDown
            className="pointer-events-none absolute right-3 top-1/2 size-4 -translate-y-1/2 text-fg-muted"
            aria-hidden="true"
          />
        </div>
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
  },
);
