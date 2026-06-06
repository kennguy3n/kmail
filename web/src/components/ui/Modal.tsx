import { useCallback, useEffect, useId, useRef } from "react";
import type { ReactNode } from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";

import { cn } from "../../lib/cn";

export type ModalSize = "sm" | "md" | "lg";

export interface ModalProps {
  open: boolean;
  onClose: () => void;
  title?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  size?: ModalSize;
  /** Close when the backdrop is clicked (default: true). */
  closeOnOverlayClick?: boolean;
  /** Close when Escape is pressed (default: true). */
  closeOnEsc?: boolean;
  /** Accessible label when no visible `title` is provided. */
  ariaLabel?: string;
}

const FOCUSABLE =
  'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])';

// Explicit rem widths (24/32/48) rather than Tailwind's named scale
// (max-w-sm/lg/3xl). The named values happen to match today, but the
// literal widths come straight from the old Modal.module.css and can't
// silently drift if the Tailwind max-width scale is ever themed.
const sizeClass: Record<ModalSize, string> = {
  sm: "max-w-[24rem]",
  md: "max-w-[32rem]",
  lg: "max-w-[48rem]",
};

// Ref-counted body scroll lock shared across all Modal instances. A
// per-modal capture/restore breaks with two open modals: when the
// first closes it would restore `overflow` to what it captured (already
// "hidden"), and a misordered close could leave the body stuck. Counting
// open modals and only touching `body.style.overflow` on the 0<->1
// transitions makes it order-independent.
let scrollLockCount = 0;
let restoreOverflow = "";

function lockBodyScroll(): void {
  if (typeof document === "undefined") return;
  if (scrollLockCount === 0) {
    restoreOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
  }
  scrollLockCount += 1;
}

function unlockBodyScroll(): void {
  if (typeof document === "undefined") return;
  scrollLockCount = Math.max(0, scrollLockCount - 1);
  if (scrollLockCount === 0) {
    document.body.style.overflow = restoreOverflow;
  }
}

/**
 * Modal — an accessible dialog rendered through a portal.
 *
 * Handles the full a11y contract for a modal: `role="dialog"` +
 * `aria-modal`, focus moved into the dialog on open and restored to
 * the trigger on close, a Tab focus-trap, Escape-to-close, and a
 * body scroll lock while open.
 */
export function Modal({
  open,
  onClose,
  title,
  children,
  footer,
  size = "md",
  closeOnOverlayClick = true,
  closeOnEsc = true,
  ariaLabel,
}: ModalProps): React.ReactPortal | null {
  const dialogRef = useRef<HTMLDivElement>(null);
  const previouslyFocused = useRef<HTMLElement | null>(null);
  const titleId = useId();

  const handleKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLDivElement>) => {
      if (event.key === "Escape" && closeOnEsc) {
        event.stopPropagation();
        onClose();
        return;
      }
      if (event.key !== "Tab") return;

      const dialog = dialogRef.current;
      if (!dialog) return;
      const focusable = Array.from(
        dialog.querySelectorAll<HTMLElement>(FOCUSABLE),
      ).filter(
        (el) => el.offsetParent !== null || el === document.activeElement,
      );
      if (focusable.length === 0) {
        event.preventDefault();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      const active = document.activeElement;

      if (event.shiftKey && active === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus();
      }
    },
    [closeOnEsc, onClose],
  );

  useEffect(() => {
    if (!open) return;

    previouslyFocused.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;

    // Lock body scroll while the modal owns the viewport (ref-counted
    // so stacked modals don't fight over `body.style.overflow`).
    lockBodyScroll();

    // Move focus into the dialog (title first, else first focusable).
    const dialog = dialogRef.current;
    const target =
      dialog?.querySelector<HTMLElement>(FOCUSABLE) ?? dialog ?? null;
    target?.focus();

    return () => {
      unlockBodyScroll();
      previouslyFocused.current?.focus?.();
    };
  }, [open]);

  if (!open || typeof document === "undefined") return null;

  return createPortal(
    <div
      className="fixed inset-0 z-modal flex items-start justify-center overflow-y-auto bg-overlay p-4 pt-[10vh] backdrop-blur-sm"
      onMouseDown={(e) => {
        if (closeOnOverlayClick && e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        className={cn(
          "flex max-h-[calc(100vh-14vh)] w-full animate-scale-in flex-col rounded-xl border border-border bg-elevated shadow-lg",
          sizeClass[size],
        )}
        role="dialog"
        aria-modal="true"
        aria-labelledby={title ? titleId : undefined}
        aria-label={!title ? ariaLabel : undefined}
        tabIndex={-1}
        onKeyDown={handleKeyDown}
      >
        {title && (
          <header className="flex shrink-0 items-center justify-between gap-4 border-b border-border px-5 py-4">
            <h2 id={titleId} className="text-lg font-semibold text-fg">
              {title}
            </h2>
            <button
              type="button"
              className="-mr-1 inline-flex size-8 shrink-0 items-center justify-center rounded-md text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              onClick={onClose}
              aria-label="Close dialog"
            >
              <X className="size-5" aria-hidden="true" />
            </button>
          </header>
        )}
        <div className="flex-1 overflow-y-auto px-5 py-4">{children}</div>
        {footer && (
          <footer className="flex shrink-0 items-center justify-end gap-2 border-t border-border px-5 py-4">
            {footer}
          </footer>
        )}
      </div>
    </div>,
    document.body,
  );
}
