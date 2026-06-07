import { useCallback, useEffect, useMemo, useState } from "react";

import { cn } from "../../lib/cn";

import {
  cancelScheduledSend,
  listScheduledSends,
  type ScheduledSendSnapshot,
  type ScheduledSendStatus,
} from "../../api/scheduledSend";

/**
 * ScheduledSends lists every scheduled message the user has
 * queued for future dispatch. Each pending row carries a Cancel
 * button; sent/cancelled/failed rows are read-only history.
 *
 * Backed by `internal/scheduledsend/handlers.go`.
 */
export default function ScheduledSends() {
  const [rows, setRows] = useState<ScheduledSendSnapshot[]>([]);
  const [isLoading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [statusMessage, setStatusMessage] = useState<string | null>(null);
  // Track cancel-in-flight per row id so the user can't double-click
  // and so the table doesn't disable every Cancel button at once.
  const [cancelling, setCancelling] = useState<Record<string, boolean>>({});

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await listScheduledSends();
      setRows(next);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const handleCancel = async (id: string) => {
    setCancelling((m) => ({ ...m, [id]: true }));
    setStatusMessage(null);
    try {
      const { cancelled } = await cancelScheduledSend(id);
      if (cancelled) {
        setStatusMessage("Scheduled send cancelled.");
      } else {
        // 410 — worker beat us to it. Reload so the row reflects
        // its true terminal state instead of leaving a stale
        // Cancel button.
        setStatusMessage(
          "Too late — the message was dispatched before cancel could land.",
        );
      }
      await reload();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setCancelling((m) => {
        const next = { ...m };
        delete next[id];
        return next;
      });
    }
  };

  const sorted = useMemo(() => {
    // Show pending first (ordered by send_at ascending so the
    // imminent ones are at the top), then everything else in
    // created-at descending. The server already orders by
    // created_at DESC; we tighten the pending bucket here so the
    // "next thing to fire" is the most discoverable row.
    const pending = rows.filter((r) => r.status === "pending");
    pending.sort((a, b) => (a.send_at < b.send_at ? -1 : a.send_at > b.send_at ? 1 : 0));
    const others = rows.filter((r) => r.status !== "pending");
    return [...pending, ...others];
  }, [rows]);

  return (
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Scheduled sends</h2>
        <button type="button" onClick={() => void reload()} className={styles.refreshButton}>
          {isLoading ? "Refreshing…" : "Refresh"}
        </button>
      </header>
      {error && (
        <div role="alert" className={styles.error}>
          <span>{error}</span>
          <button
            type="button"
            onClick={() => setError(null)}
            className={styles.errorDismiss}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {statusMessage && (
        <div role="status" className={styles.success}>
          {statusMessage}
        </div>
      )}
      {!isLoading && sorted.length === 0 && (
        <p className={styles.empty}>
          You don't have any scheduled sends yet. Open Compose, click
          the schedule menu next to Send, and pick a future time.
        </p>
      )}
      {sorted.length > 0 && (
        <table className={styles.table}>
          <thead>
            <tr>
              <th className={styles.th}>Status</th>
              <th className={styles.th}>Send at</th>
              <th className={styles.th}>Email id</th>
              <th className={styles.th}>Attempts</th>
              <th className={styles.th}>Created</th>
              <th className={styles.th}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((row) => (
              <tr key={row.id}>
                <td className={styles.td}>
                  <StatusBadge status={row.status} />
                  {row.status === "failed" && row.last_error && (
                    <div className={styles.failureDetail}>{row.last_error}</div>
                  )}
                </td>
                <td className={styles.td}>{formatTime(row.send_at)}</td>
                <td className={cn(styles.td, styles.tdMono)}>{row.email_id}</td>
                <td className={styles.td}>{row.attempts}</td>
                <td className={styles.td}>{formatTime(row.created_at)}</td>
                <td className={styles.td}>
                  {row.status === "pending" ? (
                    <button
                      type="button"
                      onClick={() => void handleCancel(row.id)}
                      disabled={!!cancelling[row.id]}
                      className={styles.cancelButton}
                      data-testid={`cancel-scheduled-${row.id}`}
                    >
                      {cancelling[row.id] ? "Cancelling…" : "Cancel"}
                    </button>
                  ) : row.status === "sent" ? (
                    <span className={styles.muted}>
                      Sent {row.sent_at ? formatTime(row.sent_at) : ""}
                    </span>
                  ) : (
                    <span className={styles.muted}>—</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function StatusBadge({ status }: { status: ScheduledSendStatus }) {
  const palette: Record<ScheduledSendStatus, string> = {
    pending: "bg-info-bg text-info-fg",
    sent: "bg-success-bg text-success-fg",
    cancelled: "bg-surface-muted text-fg-muted",
    failed: "bg-danger-bg text-danger-fg",
  };
  return (
    <span
      className={cn(
        "inline-block rounded-pill px-2 py-0.5 text-xs font-semibold uppercase tracking-wide",
        palette[status],
      )}
    >
      {status}
    </span>
  );
}

function formatTime(iso: string): string {
  // ISO strings from the BFF are RFC3339; render them in the
  // user's local timezone so "send at 9am tomorrow" reads
  // correctly rather than UTC.
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return iso;
  return t.toLocaleString();
}

/**
 * Tailwind class recipes for the Scheduled sends table view, mapped
 * onto the semantic design tokens so the page tracks the active theme.
 */
const styles: Record<string, string> = {
  root: "flex flex-col gap-4 px-6 py-4",
  header: "flex items-center justify-between gap-4",
  title: "m-0 text-2xl font-semibold",
  refreshButton:
    "cursor-pointer rounded-md border border-border bg-surface px-3 py-1.5 transition-colors hover:bg-surface-hover",
  error:
    "flex justify-between gap-3 rounded-md bg-danger-bg px-4 py-3 text-danger-fg",
  errorDismiss:
    "cursor-pointer border-0 bg-transparent text-lg leading-none text-inherit",
  success: "rounded-md bg-success-bg px-4 py-3 text-success-fg",
  empty:
    "m-0 rounded-lg border border-dashed border-border bg-surface-muted p-4 text-fg-muted",
  table: "w-full border-collapse text-sm",
  th: "border-b border-border bg-surface-muted px-3 py-2 text-left font-semibold",
  td: "border-b border-border px-3 py-2 align-top",
  tdMono: "font-mono text-xs text-fg-muted",
  cancelButton:
    "cursor-pointer rounded-md border border-warning/40 bg-warning-bg px-3 py-1 text-warning-fg transition-colors hover:bg-warning-bg/70",
  muted: "text-fg-muted",
  failureDetail: "mt-1 max-w-[32ch] break-words text-xs text-danger-fg",
};
