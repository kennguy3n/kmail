import type { ReactNode } from "react";

import styles from "./Toast.module.css";

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

const ICONS: Record<ToastVariant, string> = {
  success: "✓",
  error: "✕",
  warning: "!",
  info: "i",
};

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/**
 * Toast — a single notification card. Error and warning toasts use
 * `role="alert"` (assertive) so screen readers interrupt; success
 * and info rely on the provider's polite live region.
 */
export function Toast({ toast, onDismiss }: ToastProps): JSX.Element {
  const assertive = toast.variant === "error" || toast.variant === "warning";
  return (
    <div
      className={cx(styles.toast, styles[toast.variant])}
      role={assertive ? "alert" : "status"}
    >
      <span className={styles.icon} aria-hidden="true">
        {ICONS[toast.variant]}
      </span>
      <div className={styles.content}>
        {toast.title && <p className={styles.title}>{toast.title}</p>}
        <p className={styles.description}>{toast.description}</p>
      </div>
      <button
        type="button"
        className={styles.close}
        onClick={() => onDismiss(toast.id)}
        aria-label="Dismiss notification"
      >
        ✕
      </button>
    </div>
  );
}
