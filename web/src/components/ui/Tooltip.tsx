import { cloneElement, useId, useState } from "react";
import type { ReactElement, ReactNode } from "react";

import styles from "./Tooltip.module.css";

export type TooltipPlacement = "top" | "bottom" | "left" | "right";

export interface TooltipProps {
  /** The tooltip text/content. */
  label: ReactNode;
  /** Single focusable child the tooltip describes. */
  children: ReactElement;
  placement?: TooltipPlacement;
  className?: string;
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

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
    <span className={cx(styles.wrap, className)}>
      {child}
      {visible && (
        <span
          role="tooltip"
          id={tooltipId}
          className={cx(styles.tooltip, styles[placement])}
        >
          {label}
        </span>
      )}
    </span>
  );
}
