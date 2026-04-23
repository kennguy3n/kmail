import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import type { Email, Mailbox } from "../../types";

/**
 * Inbox is the primary Mail list view.
 *
 * The page issues JMAP `Mailbox/get` once on mount, then
 * `Email/query` + `Email/get` whenever the selected mailbox
 * changes. Phase 2 push notifications (docs/JMAP-CONTRACT.md §5)
 * are deferred to Phase 3 — state changes come from user
 * navigation for now.
 */
export default function Inbox() {
  const navigate = useNavigate();
  const { mailboxId: selectedFromRoute } = useParams<{ mailboxId?: string }>();

  const [mailboxes, setMailboxes] = useState<Mailbox[] | null>(null);
  const [emails, setEmails] = useState<Email[] | null>(null);
  const [selectedMailbox, setSelectedMailbox] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [isLoadingMailboxes, setLoadingMailboxes] = useState(true);
  const [isLoadingEmails, setLoadingEmails] = useState(false);
  // Reload nonce bumped after a write (mark-read, move-to-trash) so
  // the list refetches with the latest server state instead of
  // racing an optimistic update against the next query.
  const [reloadNonce, setReloadNonce] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoadingMailboxes(true);
    jmapClient
      .getMailboxes()
      .then((list) => {
        if (cancelled) return;
        setMailboxes(list);
        // Prefer the route-supplied mailbox if it's still valid;
        // otherwise fall back to the inbox role, then the first
        // mailbox in the list.
        const fromRoute = list.find((m) => m.id === selectedFromRoute);
        const inbox = list.find((m) => m.role === "inbox") ?? list[0];
        setSelectedMailbox((fromRoute ?? inbox)?.id ?? null);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoadingMailboxes(false);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedFromRoute, reloadNonce]);

  useEffect(() => {
    if (!selectedMailbox) {
      setEmails(null);
      return;
    }
    let cancelled = false;
    setLoadingEmails(true);
    setEmails(null);
    jmapClient
      .getEmails(selectedMailbox, { limit: 50 })
      .then((list) => {
        if (!cancelled) setEmails(list);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorMessage(err));
      })
      .finally(() => {
        if (!cancelled) setLoadingEmails(false);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedMailbox, reloadNonce]);

  const handleOpenEmail = useCallback(
    (emailId: string) => {
      if (!selectedMailbox) return;
      navigate(`/mail/${selectedMailbox}/${emailId}`);
    },
    [navigate, selectedMailbox],
  );

  const trashMailboxId = useMemo(
    () => (mailboxes ?? []).find((m) => m.role === "trash")?.id ?? null,
    [mailboxes],
  );

  const handleToggleRead = useCallback(async (email: Email) => {
    const nextRead = !email.keywords.$seen;
    try {
      await jmapClient.markRead(email.id, nextRead);
      setReloadNonce((n) => n + 1);
    } catch (err: unknown) {
      setError(errorMessage(err));
    }
  }, []);

  const handleMoveToTrash = useCallback(
    async (email: Email) => {
      if (!selectedMailbox || !trashMailboxId) {
        setError("Trash mailbox is not available on this account");
        return;
      }
      if (selectedMailbox === trashMailboxId) {
        // Already in trash: destroy permanently.
        try {
          await jmapClient.deleteEmail(email.id);
          setReloadNonce((n) => n + 1);
        } catch (err: unknown) {
          setError(errorMessage(err));
        }
        return;
      }
      try {
        await jmapClient.moveEmail(email.id, selectedMailbox, trashMailboxId);
        setReloadNonce((n) => n + 1);
      } catch (err: unknown) {
        setError(errorMessage(err));
      }
    },
    [selectedMailbox, trashMailboxId],
  );

  const sortedMailboxes = useMemo(
    () =>
      (mailboxes ?? [])
        .slice()
        .sort((a, b) => a.sortOrder - b.sortOrder || a.name.localeCompare(b.name)),
    [mailboxes],
  );

  const inTrashView =
    selectedMailbox !== null && selectedMailbox === trashMailboxId;

  return (
    <section style={layoutStyles.root}>
      <aside style={layoutStyles.sidebar}>
        <div style={layoutStyles.sidebarHeader}>
          <h2 style={layoutStyles.sidebarTitle}>Mail</h2>
          <Link to="/mail/compose" style={layoutStyles.composeButton}>
            Compose
          </Link>
        </div>
        {isLoadingMailboxes ? (
          <p style={layoutStyles.muted}>Loading mailboxes…</p>
        ) : (
          <ul style={layoutStyles.mailboxList}>
            {sortedMailboxes.map((mb) => {
              const isSelected = mb.id === selectedMailbox;
              return (
                <li key={mb.id}>
                  <button
                    type="button"
                    onClick={() => setSelectedMailbox(mb.id)}
                    style={{
                      ...layoutStyles.mailboxItem,
                      ...(isSelected ? layoutStyles.mailboxItemActive : {}),
                    }}
                  >
                    <span>{mb.name}</span>
                    {mb.unreadEmails > 0 && (
                      <span style={layoutStyles.unreadBadge}>
                        {mb.unreadEmails}
                      </span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
      </aside>
      <main style={layoutStyles.main}>
        {error && (
          <div style={layoutStyles.error}>
            <span>{error}</span>
            <button
              type="button"
              onClick={() => setError(null)}
              style={layoutStyles.errorDismiss}
              aria-label="Dismiss error"
            >
              ×
            </button>
          </div>
        )}
        {isLoadingEmails && <p style={layoutStyles.muted}>Loading emails…</p>}
        {!isLoadingEmails && emails && emails.length === 0 && (
          <p style={layoutStyles.muted}>No messages.</p>
        )}
        {emails && emails.length > 0 && (
          <ul style={layoutStyles.emailList}>
            {emails.map((email) => (
              <EmailRow
                key={email.id}
                email={email}
                inTrashView={inTrashView}
                onOpen={() => handleOpenEmail(email.id)}
                onToggleRead={() => handleToggleRead(email)}
                onMoveToTrash={() => handleMoveToTrash(email)}
              />
            ))}
          </ul>
        )}
      </main>
    </section>
  );
}

interface EmailRowProps {
  email: Email;
  inTrashView: boolean;
  onOpen: () => void;
  onToggleRead: () => void;
  onMoveToTrash: () => void;
}

function EmailRow({
  email,
  inTrashView,
  onOpen,
  onToggleRead,
  onMoveToTrash,
}: EmailRowProps) {
  const isUnread = !email.keywords.$seen;
  const from = email.from?.[0];
  const sender = from?.name ?? from?.email ?? "(unknown sender)";
  const subject = email.subject ?? "(no subject)";
  const dateLabel = formatDate(email.receivedAt);
  return (
    <li>
      <div
        style={{
          ...layoutStyles.emailRow,
          ...(isUnread ? layoutStyles.emailRowUnread : {}),
        }}
      >
        <button
          type="button"
          onClick={onOpen}
          style={layoutStyles.emailRowMain}
        >
          <span style={layoutStyles.emailSender}>{sender}</span>
          <span style={layoutStyles.emailSubject}>{subject}</span>
          <span style={layoutStyles.emailDate}>{dateLabel}</span>
        </button>
        <div style={layoutStyles.emailActions}>
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onToggleRead();
            }}
            style={layoutStyles.actionButton}
            title={isUnread ? "Mark as read" : "Mark as unread"}
          >
            {isUnread ? "Mark read" : "Mark unread"}
          </button>
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onMoveToTrash();
            }}
            style={layoutStyles.actionButton}
            title={inTrashView ? "Delete permanently" : "Move to trash"}
          >
            {inTrashView ? "Delete" : "Trash"}
          </button>
        </div>
      </div>
    </li>
  );
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return "Unknown error";
}

function formatDate(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const now = new Date();
  const sameDay =
    d.getFullYear() === now.getFullYear() &&
    d.getMonth() === now.getMonth() &&
    d.getDate() === now.getDate();
  return sameDay
    ? d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString();
}

const layoutStyles: Record<string, React.CSSProperties> = {
  root: {
    display: "grid",
    gridTemplateColumns: "220px 1fr",
    minHeight: "calc(100vh - 4rem)",
    gap: "1rem",
  },
  sidebar: {
    borderRight: "1px solid #e5e7eb",
    padding: "1rem",
    background: "#f9fafb",
  },
  sidebarHeader: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.75rem",
  },
  sidebarTitle: {
    margin: 0,
    fontSize: "1.1rem",
  },
  composeButton: {
    padding: "0.25rem 0.5rem",
    fontSize: "0.85rem",
    background: "#2563eb",
    color: "#fff",
    borderRadius: "0.25rem",
    textDecoration: "none",
  },
  mailboxList: {
    listStyle: "none",
    margin: 0,
    padding: 0,
    display: "flex",
    flexDirection: "column",
    gap: "0.125rem",
  },
  mailboxItem: {
    display: "flex",
    justifyContent: "space-between",
    alignItems: "center",
    width: "100%",
    padding: "0.35rem 0.5rem",
    background: "transparent",
    border: "none",
    textAlign: "left",
    cursor: "pointer",
    borderRadius: "0.25rem",
    fontSize: "0.9rem",
  },
  mailboxItemActive: {
    background: "#dbeafe",
    fontWeight: 600,
  },
  unreadBadge: {
    background: "#2563eb",
    color: "#fff",
    fontSize: "0.7rem",
    padding: "0.05rem 0.35rem",
    borderRadius: "999px",
  },
  main: {
    padding: "1rem",
  },
  error: {
    padding: "0.5rem 0.75rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    marginBottom: "0.75rem",
  },
  muted: {
    color: "#6b7280",
    fontStyle: "italic",
  },
  emailList: {
    listStyle: "none",
    margin: 0,
    padding: 0,
    borderTop: "1px solid #e5e7eb",
  },
  emailRow: {
    display: "grid",
    gridTemplateColumns: "180px 1fr 120px",
    alignItems: "center",
    gap: "0.75rem",
    width: "100%",
    padding: "0.6rem 0.5rem",
    background: "transparent",
    border: "none",
    borderBottom: "1px solid #e5e7eb",
    textAlign: "left",
    cursor: "pointer",
    fontSize: "0.9rem",
  },
  emailRowUnread: {
    fontWeight: 600,
    background: "#eff6ff",
  },
  emailSender: {
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
  },
  emailSubject: {
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
    color: "#111827",
  },
  emailDate: {
    textAlign: "right",
    color: "#6b7280",
    fontSize: "0.8rem",
  },
};
