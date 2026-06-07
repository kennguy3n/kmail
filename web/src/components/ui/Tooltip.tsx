import { cloneElement, useId, useState } from "react";
import type { ReactElement, ReactNode } from "react";

import { cn } from "../../lib/cn";

export type TooltipPlacement = "top" | "bottom" | "left" | "right";

export interface TooltipProps {
  /** The tooltip text/content. */
  label: ReactNode;
  /** Single focusable child the tooltip describes. */
  children: ReactElement;
  placement?: TooltipPlacement;
  className?: string;
}

const placementClass: Record<TooltipPlacement, string> = {
  top: "bottom-[calc(100%+0.4rem)] left-1/2 -translate-x-1/2",
  bottom: "top-[calc(100%+0.4rem)] left-1/2 -translate-x-1/2",
  left: "right-[calc(100%+0.4rem)] top-1/2 -translate-y-1/2",
  right: "left-[calc(100%+0.4rem)] top-1/2 -translate-y-1/2",
};

/**
 * Tooltip — shows supplementary text on hover/focus. The trigger is
 * linked to the tooltip via `aria-describedby`, and the tooltip is
 * dismissible on Escape per WCAG 1.4.13. It does not trap focus or
 * steal it — the child remains the interactive element.
 */
export function Tooltip({
  label,
  children,
  placement = "top",
  className,
}: TooltipProps): JSX.Element {
  const [visible, setVisible] = useState(false);
  const tooltipId = useId();

  const show = (): void => setVisible(true);
  const hide = (): void => setVisible(false);

  const child = cloneElement(children, {
    "aria-describedby": visible ? tooltipId : undefined,
    onMouseEnter: (e: React.MouseEvent) => {
      children.props.onMouseEnter?.(e);
      show();
    },
    onMouseLeave: (e: React.MouseEvent) => {
      children.props.onMouseLeave?.(e);
      hide();
    },
    onFocus: (e: React.FocusEvent) => {
      children.props.onFocus?.(e);
      show();
    },
    onBlur: (e: React.FocusEvent) => {
      children.props.onBlur?.(e);
      hide();
    },
    onKeyDown: (e: React.KeyboardEvent) => {
      children.props.onKeyDown?.(e);
      if (e.key === "Escape") hide();
    },
  });

  return (
    <span className={cn("relative inline-flex", className)}>
      {child}
      {visible && (
        <span
          role="tooltip"
          id={tooltipId}
          className={cn(
            "pointer-events-none absolute z-tooltip w-max max-w-xs animate-fade-in rounded-md bg-fg px-2 py-1 text-xs font-medium text-fg-inverse shadow-md",
            placementClass[placement],
          )}
        >
          {label}
        </span>
      )}
    </span>
  );
}
