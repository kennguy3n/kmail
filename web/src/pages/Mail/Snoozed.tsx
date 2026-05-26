import { useCallback, useEffect, useMemo, useState } from "react";

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
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Snoozed emails</h2>
        <button
          type="button"
          onClick={() => void reload()}
          style={styles.refreshButton}
        >
          {isLoading ? "Refreshing…" : "Refresh"}
        </button>
      </header>
      {error && (
        <div role="alert" style={styles.error}>
          <span>{error}</span>
          <button
            type="button"
            onClick={() => setError(null)}
            style={styles.errorDismiss}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {statusMessage && (
        <div role="status" style={styles.success}>
          {statusMessage}
        </div>
      )}
      {!isLoading && sorted.length === 0 && (
        <p style={styles.empty}>
          You don't have any snoozed emails. Open any message in your
          inbox, click "Snooze", and pick a time to return.
        </p>
      )}
      {sorted.length > 0 && (
        <table style={styles.table}>
          <thead>
            <tr>
              <th style={styles.th}>Status</th>
              <th style={styles.th}>Wakes at</th>
              <th style={styles.th}>Email id</th>
              <th style={styles.th}>Attempts</th>
              <th style={styles.th}>Created</th>
              <th style={styles.th}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((row) => (
              <tr key={row.id}>
                <td style={styles.td}>
                  <StatusBadge status={row.status} />
                  {row.status === "failed" && row.last_error && (
                    <div style={styles.failureDetail}>{row.last_error}</div>
                  )}
                </td>
                <td style={styles.td}>{formatTime(row.snooze_until)}</td>
                <td style={{ ...styles.td, ...styles.tdMono }}>{row.email_id}</td>
                <td style={styles.td}>{row.attempts}</td>
                <td style={styles.td}>{formatTime(row.created_at)}</td>
                <td style={styles.td}>
                  {row.status === "snoozed" ? (
                    <button
                      type="button"
                      onClick={() => void handleWake(row.id)}
                      disabled={!!waking[row.id]}
                      style={styles.wakeButton}
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
                      style={styles.wakeButton}
                      data-testid={`retry-snooze-${row.id}`}
                    >
                      {waking[row.id] ? "Retrying…" : "Retry wake"}
                    </button>
                  ) : row.status === "unsnoozed" ? (
                    <span style={styles.muted}>
                      Woke {row.woken_at ? formatTime(row.woken_at) : ""}
                    </span>
                  ) : (
                    <span style={styles.muted}>—</span>
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
  const palette: Record<SnoozeStatus, React.CSSProperties> = {
    snoozed: { background: "#e0f2fe", color: "#0369a1" },
    unsnoozed: { background: "#dcfce7", color: "#15803d" },
    cancelled: { background: "#f3f4f6", color: "#4b5563" },
    failed: { background: "#fee2e2", color: "#b91c1c" },
  };
  return (
    <span
      style={{
        display: "inline-block",
        padding: "0.125rem 0.5rem",
        borderRadius: "999px",
        fontSize: "0.75rem",
        fontWeight: 600,
        textTransform: "uppercase",
        letterSpacing: "0.04em",
        ...palette[status],
      }}
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

const styles = {
  root: {
    padding: "1rem 1.5rem",
    display: "flex",
    flexDirection: "column" as const,
    gap: "1rem",
  },
  header: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: "1rem",
  },
  title: { margin: 0, fontSize: "1.5rem" },
  refreshButton: {
    padding: "0.375rem 0.75rem",
    background: "#f9fafb",
    border: "1px solid #d1d5db",
    borderRadius: "0.375rem",
    cursor: "pointer",
  },
  error: {
    padding: "0.75rem 1rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.375rem",
    display: "flex",
    justifyContent: "space-between",
    gap: "0.75rem",
  },
  errorDismiss: {
    background: "transparent",
    border: "none",
    color: "inherit",
    cursor: "pointer",
    fontSize: "1.125rem",
    lineHeight: 1,
  },
  success: {
    padding: "0.75rem 1rem",
    background: "#dcfce7",
    color: "#166534",
    borderRadius: "0.375rem",
  },
  empty: {
    padding: "1rem",
    background: "#f9fafb",
    border: "1px dashed #d1d5db",
    borderRadius: "0.5rem",
    color: "#4b5563",
    margin: 0,
  },
  table: {
    width: "100%",
    borderCollapse: "collapse" as const,
    fontSize: "0.95rem",
  },
  th: {
    textAlign: "left" as const,
    padding: "0.5rem 0.75rem",
    background: "#f3f4f6",
    borderBottom: "1px solid #d1d5db",
    fontWeight: 600,
  },
  td: {
    padding: "0.5rem 0.75rem",
    borderBottom: "1px solid #f3f4f6",
    verticalAlign: "top" as const,
  },
  tdMono: {
    fontFamily: "ui-monospace, monospace",
    fontSize: "0.85rem",
    color: "#374151",
  },
  wakeButton: {
    padding: "0.25rem 0.75rem",
    background: "#eef2ff",
    color: "#3730a3",
    border: "1px solid #c7d2fe",
    borderRadius: "0.375rem",
    cursor: "pointer",
  },
  muted: { color: "#6b7280" },
  failureDetail: {
    marginTop: "0.25rem",
    fontSize: "0.8rem",
    color: "#7f1d1d",
    maxWidth: "32ch",
    overflowWrap: "break-word" as const,
  },
};
