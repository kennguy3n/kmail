import { useCallback, useEffect, useMemo, useState } from "react";

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
    <section style={styles.root}>
      <header style={styles.header}>
        <h2 style={styles.title}>Scheduled sends</h2>
        <button type="button" onClick={() => void reload()} style={styles.refreshButton}>
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
          You don't have any scheduled sends yet. Open Compose, click
          the schedule menu next to Send, and pick a future time.
        </p>
      )}
      {sorted.length > 0 && (
        <table style={styles.table}>
          <thead>
            <tr>
              <th style={styles.th}>Status</th>
              <th style={styles.th}>Send at</th>
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
                <td style={styles.td}>{formatTime(row.send_at)}</td>
                <td style={{ ...styles.td, ...styles.tdMono }}>{row.email_id}</td>
                <td style={styles.td}>{row.attempts}</td>
                <td style={styles.td}>{formatTime(row.created_at)}</td>
                <td style={styles.td}>
                  {row.status === "pending" ? (
                    <button
                      type="button"
                      onClick={() => void handleCancel(row.id)}
                      disabled={!!cancelling[row.id]}
                      style={styles.cancelButton}
                      data-testid={`cancel-scheduled-${row.id}`}
                    >
                      {cancelling[row.id] ? "Cancelling…" : "Cancel"}
                    </button>
                  ) : row.status === "sent" ? (
                    <span style={styles.muted}>
                      Sent {row.sent_at ? formatTime(row.sent_at) : ""}
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

function StatusBadge({ status }: { status: ScheduledSendStatus }) {
  const palette: Record<ScheduledSendStatus, React.CSSProperties> = {
    pending: { background: "#e0f2fe", color: "#0369a1" },
    sent: { background: "#dcfce7", color: "#15803d" },
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
  // ISO strings from the BFF are RFC3339; render them in the
  // user's local timezone so "send at 9am tomorrow" reads
  // correctly rather than UTC.
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
  cancelButton: {
    padding: "0.25rem 0.75rem",
    background: "#fff7ed",
    color: "#9a3412",
    border: "1px solid #fdba74",
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
