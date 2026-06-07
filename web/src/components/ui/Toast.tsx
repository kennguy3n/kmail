import type { ReactNode } from "react";
import {
  CheckCircle2,
  Info,
  TriangleAlert,
  X,
  XCircle,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";

import { cn } from "../../lib/cn";

export type ToastVariant = "success" | "error" | "warning" | "info";

export interface ToastData {
  id: string;
  title?: ReactNode;
  description: ReactNode;
  variant: ToastVariant;
  /** ms before auto-dismiss; 0 / Infinity disables it. */
  duration: number;
}

export interface ToastProps {
  toast: ToastData;
  onDismiss: (id: string) => void;
}

const ICONS: Record<ToastVariant, LucideIcon> = {
  success: CheckCircle2,
  error: XCircle,
  warning: TriangleAlert,
  info: Info,
};

const accent: Record<ToastVariant, string> = {
  success: "text-success",
  error: "text-danger",
  warning: "text-warning",
  info: "text-info",
};

// 4px variant-coloured left border (matches the old Toast.module.css).
// Pairs with the icon colour so the toast's variant stays salient even
// if the small icon hue is missed.
const leftBorder: Record<ToastVariant, string> = {
  success: "border-l-success",
  error: "border-l-danger",
  warning: "border-l-warning",
  info: "border-l-info",
};

/**
 * Toast — a single notification card. Error and warning toasts use
 * `role="alert"` (assertive) so screen readers interrupt; success and
 * info use `role="status"` (polite). Each toast is its own live region,
 * so the provider's container deliberately has no `aria-live` (that
 * would double-announce).
 */
export function Toast({ toast, onDismiss }: ToastProps): JSX.Element {
  const assertive = toast.variant === "error" || toast.variant === "warning";
  const Icon = ICONS[toast.variant];
  return (
    <div
      className={cn(
        "pointer-events-auto flex w-80 animate-slide-in-right items-start gap-3 rounded-lg border border-l-4 border-border bg-elevated p-3.5 shadow-lg",
        leftBorder[toast.variant],
      )}
      role={assertive ? "alert" : "status"}
    >
      <Icon
        className={cn("mt-0.5 size-5 shrink-0", accent[toast.variant])}
        aria-hidden="true"
      />
      <div className="min-w-0 flex-1">
        {toast.title && (
          <p className="text-sm font-semibold text-fg">{toast.title}</p>
        )}
        <p className="text-sm text-fg-muted">{toast.description}</p>
      </div>
      <button
        type="button"
        className="-mr-1 -mt-1 inline-flex size-7 shrink-0 items-center justify-center rounded-md text-fg-subtle transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        onClick={() => onDismiss(toast.id)}
        aria-label="Dismiss notification"
      >
        <X className="size-4" aria-hidden="true" />
      </button>
    </div>
  );
}
