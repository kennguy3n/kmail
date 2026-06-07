import { useCallback, useEffect, useMemo, useState } from "react";

import { cn } from "../../lib/cn";

import {
  listSnoozes,
  wakeSnooze,
  type SnoozeSnapshot,
  type SnoozeStatus,
} from "../../api/snooze";

/**
 * Snoozed lists every snoozed email the user has queued for
 * future wake. Each row in `snoozed` status carries a "Wake now"
 * button; terminal rows are read-only history.
 *
 * Backed by `internal/snooze/handlers.go`.
 */
export default function Snoozed() {
  const [rows, setRows] = useState<SnoozeSnapshot[]>([]);
  const [isLoading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [statusMessage, setStatusMessage] = useState<string | null>(null);
  const [waking, setWaking] = useState<Record<string, boolean>>({});

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await listSnoozes();
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

  // handleWake is shared by the "Wake now" button (snoozed rows)
  // AND the "Retry wake" button (failed rows). Both call the
  // same DELETE /api/v1/snoozed/{id} endpoint; the backend
  // distinguishes the two cases internally (failed rows fall
  // through to applyMove + Cancel just like snoozed rows). The
  // distinct verb / success copy is purely cosmetic — the
  // semantics are: "move this email back to its inbox now".
  const handleWake = async (id: string, retrying = false) => {
    setWaking((m) => ({ ...m, [id]: true }));
    setStatusMessage(null);
    try {
      await wakeSnooze(id);
      setStatusMessage(
        retrying
          ? "Wake retried — the email is back in its mailbox."
          : "Snooze cancelled — the email is back in its mailbox.",
      );
      await reload();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setWaking((m) => {
        const next = { ...m };
        delete next[id];
        return next;
      });
    }
  };

  const sorted = useMemo(() => {
    // Active snoozes first, ordered by snooze_until ASC so the
    // next-to-wake row sits at the top. Terminal rows fall back
    // to created_at DESC (the server already ships them that way).
    const active = rows.filter((r) => r.status === "snoozed");
    active.sort((a, b) =>
      a.snooze_until < b.snooze_until ? -1 : a.snooze_until > b.snooze_until ? 1 : 0,
    );
    const others = rows.filter((r) => r.status !== "snoozed");
    return [...active, ...others];
  }, [rows]);

  return (
    <section className={styles.root}>
      <header className={styles.header}>
        <h2 className={styles.title}>Snoozed emails</h2>
        <button
          type="button"
          onClick={() => void reload()}
          className={styles.refreshButton}
        >
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
          You don't have any snoozed emails. Open any message in your
          inbox, click "Snooze", and pick a time to return.
        </p>
      )}
      {sorted.length > 0 && (
        <table className={styles.table}>
          <thead>
            <tr>
              <th className={styles.th}>Status</th>
              <th className={styles.th}>Wakes at</th>
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
                <td className={styles.td}>{formatTime(row.snooze_until)}</td>
                <td className={cn(styles.td, styles.tdMono)}>{row.email_id}</td>
                <td className={styles.td}>{row.attempts}</td>
                <td className={styles.td}>{formatTime(row.created_at)}</td>
                <td className={styles.td}>
                  {row.status === "snoozed" ? (
                    <button
                      type="button"
                      onClick={() => void handleWake(row.id)}
                      disabled={!!waking[row.id]}
                      className={styles.wakeButton}
                      data-testid={`wake-snooze-${row.id}`}
                    >
                      {waking[row.id] ? "Waking…" : "Wake now"}
                    </button>
                  ) : row.status === "failed" ? (
                    // The worker exhausted retries and gave up;
                    // the email is still stuck in the Snoozed
                    // folder. "Retry wake" gives the user a
                    // self-service path to re-attempt the JMAP
                    // move (and, on success, flip the row to
                    // cancelled). Without this, users would
                    // have to ask an operator to manually patch
                    // mailboxIds in Stalwart.
                    <button
                      type="button"
                      onClick={() => void handleWake(row.id, true)}
                      disabled={!!waking[row.id]}
                      className={styles.wakeButton}
                      data-testid={`retry-snooze-${row.id}`}
                    >
                      {waking[row.id] ? "Retrying…" : "Retry wake"}
                    </button>
                  ) : row.status === "unsnoozed" ? (
                    <span className={styles.muted}>
                      Woke {row.woken_at ? formatTime(row.woken_at) : ""}
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

function StatusBadge({ status }: { status: SnoozeStatus }) {
  const palette: Record<SnoozeStatus, string> = {
    snoozed: "bg-info-bg text-info-fg",
    unsnoozed: "bg-success-bg text-success-fg",
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
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return iso;
  return t.toLocaleString();
}

/**
 * Tailwind class recipes for the Snoozed table view, mapped onto the
 * semantic design tokens so the page tracks the active theme.
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
  wakeButton:
    "cursor-pointer rounded-md border border-primary/30 bg-primary-subtle px-3 py-1 text-primary transition-colors hover:bg-primary-subtle/70",
  muted: "text-fg-muted",
  failureDetail: "mt-1 max-w-[32ch] break-words text-xs text-danger-fg",
};
