import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";

import { Toast } from "./ui/Toast";
import type { ToastData, ToastVariant } from "./ui/Toast";
import styles from "./ToastProvider.module.css";

export interface ToastOptions {
  /** Provide to deduplicate / update an existing toast. */
  id?: string;
  title?: ReactNode;
  variant?: ToastVariant;
  /** ms before auto-dismiss. Use 0 (or Infinity) to make it sticky. */
  duration?: number;
}

export interface ToastApi {
  /** Show a toast; returns its id so callers can dismiss it early. */
  toast: (description: ReactNode, options?: ToastOptions) => string;
  success: (description: ReactNode, options?: ToastOptions) => string;
  error: (description: ReactNode, options?: ToastOptions) => string;
  warning: (description: ReactNode, options?: ToastOptions) => string;
  info: (description: ReactNode, options?: ToastOptions) => string;
  dismiss: (id: string) => void;
  dismissAll: () => void;
}

const DEFAULT_DURATION_MS = 5000;
/** Errors stay longer by default so they aren't missed. */
const DEFAULT_ERROR_DURATION_MS = 8000;

const ToastContext = createContext<ToastApi | null>(null);

let idCounter = 0;
function nextId(): string {
  idCounter += 1;
  return `toast-${idCounter}`;
}

export interface ToastProviderProps {
  children: ReactNode;
  /** Max simultaneously visible toasts (oldest dropped first). */
  max?: number;
}

/**
 * ToastProvider — app-wide notification surface.
 *
 * Wrap the tree once (e.g. in `main.tsx`) and call {@link useToast}
 * anywhere to raise success/error/warning/info messages. This
 * centralizes the per-page `setError(...)` pattern behind a single
 * accessible, auto-dismissing surface with an `aria-live` region.
 */
export function ToastProvider({
  children,
  max = 4,
}: ToastProviderProps): JSX.Element {
  const [toasts, setToasts] = useState<ToastData[]>([]);
  // Track auto-dismiss timers so we can clear them on manual dismiss
  // / unmount and never leak or fire after removal.
  const timers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());

  const clearTimer = useCallback((id: string) => {
    const timer = timers.current.get(id);
    if (timer) {
      clearTimeout(timer);
      timers.current.delete(id);
    }
  }, []);

  const dismiss = useCallback(
    (id: string) => {
      clearTimer(id);
      setToasts((prev) => prev.filter((t) => t.id !== id));
    },
    [clearTimer],
  );

  const push = useCallback(
    (
      description: ReactNode,
      variant: ToastVariant,
      options?: ToastOptions,
    ): string => {
      const id = options?.id ?? nextId();
      const duration =
        options?.duration ??
        (variant === "error"
          ? DEFAULT_ERROR_DURATION_MS
          : DEFAULT_DURATION_MS);

      const data: ToastData = {
        id,
        title: options?.title,
        description,
        variant,
        duration,
      };

      setToasts((prev) => {
        // Replace in place when an id is reused (update semantics).
        const existing = prev.findIndex((t) => t.id === id);
        let next: ToastData[];
        if (existing >= 0) {
          next = [...prev];
          next[existing] = data;
        } else {
          next = [...prev, data];
        }
        // Enforce the cap by dropping the oldest.
        if (next.length > max) {
          const overflow = next.slice(0, next.length - max);
          overflow.forEach((t) => clearTimer(t.id));
          next = next.slice(next.length - max);
        }
        return next;
      });

      // (Re)arm the auto-dismiss timer.
      clearTimer(id);
      if (duration > 0 && Number.isFinite(duration)) {
        timers.current.set(
          id,
          setTimeout(() => dismiss(id), duration),
        );
      }
      return id;
    },
    [clearTimer, dismiss, max],
  );

  const dismissAll = useCallback(() => {
    timers.current.forEach((timer) => clearTimeout(timer));
    timers.current.clear();
    setToasts([]);
  }, []);

  // Clear any outstanding timers when the provider unmounts.
  useEffect(() => {
    const map = timers.current;
    return () => {
      map.forEach((timer) => clearTimeout(timer));
      map.clear();
    };
  }, []);

  const api = useMemo<ToastApi>(
    () => ({
      toast: (description, options) =>
        push(description, options?.variant ?? "info", options),
      success: (description, options) => push(description, "success", options),
      error: (description, options) => push(description, "error", options),
      warning: (description, options) => push(description, "warning", options),
      info: (description, options) => push(description, "info", options),
      dismiss,
      dismissAll,
    }),
    [push, dismiss, dismissAll],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      {/* No `aria-live` on this container: each Toast carries its own
          role (`alert` for error/warning = assertive, `status` for
          success/info = polite), which is the single source of
          announcements. Wrapping role="alert" toasts inside a polite
          live region made some screen readers (e.g. NVDA) announce
          twice. The container is a positioning/grouping element only. */}
      <div className={styles.viewport} role="region" aria-label="Notifications">
        {toasts.map((t) => (
          <Toast key={t.id} toast={t} onDismiss={dismiss} />
        ))}
      </div>
    </ToastContext.Provider>
  );
}

/** Access the toast API. Must be called under a {@link ToastProvider}. */
export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) {
    throw new Error("useToast must be used within a <ToastProvider>");
  }
  return ctx;
}
