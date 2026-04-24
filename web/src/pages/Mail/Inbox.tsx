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

  // Search state. `query` is the input value; `submittedQuery` is
  // the last value actually sent to the server and is what drives
  // whether the main pane shows search results or a mailbox
  // listing. `searchScope` toggles between scoping the search to
  // the sidebar-selected mailbox and searching every mailbox the
  // user can see. A non-empty `submittedQuery` puts the page into
  // search mode; clearing it returns to the normal mailbox view
  // without re-querying the server.
  const [query, setQuery] = useState("");
  const [submittedQuery, setSubmittedQuery] = useState("");
  const [searchScope, setSearchScope] = useState<"mailbox" | "global">(
    "mailbox",
  );
  // Scope captured at the moment the last search was submitted;
  // `searchScope` above is the live checkbox state and can diverge
  // from what the currently-displayed results were actually
  // searched under.
  const [submittedScope, setSubmittedScope] = useState<"mailbox" | "global">(
    "mailbox",
  );
  const [searchResults, setSearchResults] = useState<Email[] | null>(null);
  const [isSearching, setIsSearching] = useState(false);
  // Bumped after every successful write (mark-read, move-to-trash,
  // delete) so the search-mode refetch effect below re-runs the
  // last submitted search and replaces stale hits with server
  // state. `reloadNonce` still drives the mailbox-mode refetch.
  const [searchReloadNonce, setSearchReloadNonce] = useState(0);
  const inSearchMode = submittedQuery.trim().length > 0;

  useEffect(() => {
    let cancelled = false;
    setLoadingMailboxes(true);
    jmapClient
      .getMailboxes()
      .then((list) => {
        if (cancelled) return;
        setMailboxes(list);
        // Prefer the route-supplied mailbox when present; otherwise
        // keep the user's current sidebar selection if it still
        // exists, then fall back to the inbox role, then the first
        // mailbox in the list. Preserving the current selection
        // matters when `reloadNonce` triggers a refetch — without
        // it, the view would snap back to the Inbox after any
        // write action in a sidebar-selected mailbox.
        const fromRoute = list.find((m) => m.id === selectedFromRoute);
        const inbox = list.find((m) => m.role === "inbox") ?? list[0];
        setSelectedMailbox((current) => {
          if (fromRoute) return fromRoute.id;
          if (current && list.some((m) => m.id === current)) return current;
          return inbox?.id ?? null;
        });
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
      // In search mode the result may belong to a different
      // mailbox than the sidebar selection; pick the first mailbox
      // id on the email so the MessageView URL is always valid.
      let mailboxId: string | null = selectedMailbox;
      if (inSearchMode) {
        const hit = (searchResults ?? []).find((e) => e.id === emailId);
        const firstOnEmail = hit ? Object.keys(hit.mailboxIds)[0] : undefined;
        mailboxId = firstOnEmail ?? selectedMailbox;
      }
      if (!mailboxId) return;
      navigate(`/mail/${mailboxId}/${emailId}`);
    },
    [inSearchMode, navigate, searchResults, selectedMailbox],
  );

  const runSearch = useCallback(
    async (raw: string) => {
      const trimmed = raw.trim();
      setSubmittedQuery(trimmed);
      setSubmittedScope(searchScope);
      if (trimmed.length === 0) {
        setSearchResults(null);
        return;
      }
      setIsSearching(true);
      try {
        const results = await jmapClient.searchEmails(trimmed, {
          mailboxId:
            searchScope === "mailbox" ? (selectedMailbox ?? undefined) : null,
          limit: 50,
        });
        setSearchResults(results);
      } catch (err: unknown) {
        setError(errorMessage(err));
        setSearchResults([]);
      } finally {
        setIsSearching(false);
      }
    },
    [searchScope, selectedMailbox],
  );

  // After a successful write in search mode, re-run the last
  // submitted search against the captured `submittedScope` so
  // `searchResults` converges with server state. Using
  // `submittedScope` (not the live `searchScope`) matches what the
  // currently-displayed results were actually queried under.
  useEffect(() => {
    if (searchReloadNonce === 0) return;
    if (!inSearchMode) return;
    let cancelled = false;
    setIsSearching(true);
    jmapClient
      .searchEmails(submittedQuery, {
        mailboxId:
          submittedScope === "mailbox"
            ? (selectedMailbox ?? undefined)
            : null,
        limit: 50,
      })
      .then((results) => {
        if (!cancelled) setSearchResults(results);
      })
      .catch((err: unknown) => {
        if (!cancelled) {
          setError(errorMessage(err));
          setSearchResults([]);
        }
      })
      .finally(() => {
        if (!cancelled) setIsSearching(false);
      });
    return () => {
      cancelled = true;
    };
  }, [
    searchReloadNonce,
    inSearchMode,
    submittedQuery,
    submittedScope,
    selectedMailbox,
  ]);

  const handleSubmitSearch = useCallback(
    (e: React.FormEvent) => {
      e.preventDefault();
      void runSearch(query);
    },
    [query, runSearch],
  );

  const handleClearSearch = useCallback(() => {
    setQuery("");
    setSubmittedQuery("");
    setSearchResults(null);
  }, []);

  const trashMailboxId = useMemo(
    () => (mailboxes ?? []).find((m) => m.role === "trash")?.id ?? null,
    [mailboxes],
  );

  // Single source of truth for "this row behaves as if it lives in
  // trash". Used both for the row label (Trash vs Delete) and the
  // handler's delete-vs-move branch so they can't drift. In search
  // mode, results can come from any mailbox so the decision is
  // per-email; outside search mode, the user is viewing a specific
  // mailbox and the old sidebar-based rule applies (so a message
  // cross-labelled Inbox+Trash still moves when the user clicks
  // Trash from Inbox, matching the pre-search-feature behaviour).
  const isEmailInTrash = useCallback(
    (email: Email): boolean => {
      if (trashMailboxId === null) return false;
      if (inSearchMode) {
        return Object.prototype.hasOwnProperty.call(
          email.mailboxIds,
          trashMailboxId,
        );
      }
      return selectedMailbox === trashMailboxId;
    },
    [inSearchMode, selectedMailbox, trashMailboxId],
  );

  // Bump both refetch nonces after a successful write. The
  // mailbox-list effect reads `reloadNonce`; the search effect
  // above reads `searchReloadNonce`. Bumping both here keeps the
  // page converged regardless of which list is currently on screen.
  const bumpAfterWrite = useCallback(() => {
    setReloadNonce((n) => n + 1);
    setSearchReloadNonce((n) => n + 1);
  }, []);

  const handleToggleRead = useCallback(
    async (email: Email) => {
      const nextRead = !email.keywords.$seen;
      try {
        await jmapClient.markRead(email.id, nextRead);
        bumpAfterWrite();
      } catch (err: unknown) {
        setError(errorMessage(err));
      }
    },
    [bumpAfterWrite],
  );

  const handleMoveToTrash = useCallback(
    async (email: Email) => {
      if (!trashMailboxId) {
        setError("Trash mailbox is not available on this account");
        return;
      }
      if (isEmailInTrash(email)) {
        try {
          await jmapClient.deleteEmail(email.id);
          bumpAfterWrite();
        } catch (err: unknown) {
          setError(errorMessage(err));
        }
        return;
      }
      // Resolve the source mailbox from the email itself in search
      // mode so the JMAP patch removes it from its actual location
      // rather than a no-op key on the sidebar selection.
      const emailMailboxIds = Object.keys(email.mailboxIds);
      const sourceMailbox = inSearchMode
        ? (emailMailboxIds[0] ?? selectedMailbox)
        : selectedMailbox;
      if (!sourceMailbox) {
        setError("Could not determine source mailbox for this email");
        return;
      }
      try {
        await jmapClient.moveEmail(email.id, sourceMailbox, trashMailboxId);
        bumpAfterWrite();
      } catch (err: unknown) {
        setError(errorMessage(err));
      }
    },
    [bumpAfterWrite, inSearchMode, isEmailInTrash, selectedMailbox, trashMailboxId],
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
        <form style={layoutStyles.searchBar} onSubmit={handleSubmitSearch}>
          <input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search mail…"
            aria-label="Search mail"
            style={layoutStyles.searchInput}
          />
          <label style={layoutStyles.searchScopeLabel}>
            <input
              type="checkbox"
              checked={searchScope === "global"}
              onChange={(e) =>
                setSearchScope(e.target.checked ? "global" : "mailbox")
              }
            />
            All mailboxes
          </label>
          <button type="submit" style={layoutStyles.searchButton}>
            Search
          </button>
          {inSearchMode && (
            <button
              type="button"
              onClick={handleClearSearch}
              style={layoutStyles.searchClear}
            >
              Clear
            </button>
          )}
        </form>
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
        {inSearchMode && (
          <p style={layoutStyles.searchStatus}>
            {isSearching
              ? `Searching for “${submittedQuery}”…`
              : `Results for “${submittedQuery}” (${
                  searchResults?.length ?? 0
                })${
                  submittedScope === "mailbox"
                    ? " in this mailbox"
                    : " across all mail"
                }`}
          </p>
        )}
        {!inSearchMode && isLoadingEmails && (
          <p style={layoutStyles.muted}>Loading emails…</p>
        )}
        {!inSearchMode &&
          !isLoadingEmails &&
          emails &&
          emails.length === 0 && (
            <p style={layoutStyles.muted}>No messages.</p>
          )}
        {inSearchMode &&
          !isSearching &&
          searchResults &&
          searchResults.length === 0 && (
            <p style={layoutStyles.muted}>No matching messages.</p>
          )}
        {(() => {
          const list = inSearchMode ? (searchResults ?? []) : (emails ?? []);
          if (list.length === 0) return null;
          return (
            <ul style={layoutStyles.emailList}>
              {list.map((email) => {
                // In search mode the sidebar mailbox is not
                // authoritative, so compute per-email whether the
                // hit already lives in trash (which is what
                // handleMoveToTrash keys off). Outside search mode
                // the sidebar flag is correct and cheaper.
                const rowInTrash = inSearchMode
                  ? trashMailboxId !== null &&
                    Object.prototype.hasOwnProperty.call(
                      email.mailboxIds,
                      trashMailboxId,
                    )
                  : inTrashView;
                return (
                  <EmailRow
                    key={email.id}
                    email={email}
                    inTrashView={rowInTrash}
                    onOpen={() => handleOpenEmail(email.id)}
                    onToggleRead={() => handleToggleRead(email)}
                    onMoveToTrash={() => handleMoveToTrash(email)}
                  />
                );
              })}
            </ul>
          );
        })()}
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
  searchBar: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    marginBottom: "0.75rem",
  },
  searchInput: {
    flex: 1,
    padding: "0.4rem 0.6rem",
    fontSize: "0.9rem",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  searchScopeLabel: {
    display: "flex",
    alignItems: "center",
    gap: "0.25rem",
    fontSize: "0.8rem",
    color: "#374151",
  },
  searchButton: {
    padding: "0.4rem 0.75rem",
    fontSize: "0.85rem",
    background: "#2563eb",
    color: "#fff",
    border: "none",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  searchClear: {
    padding: "0.4rem 0.75rem",
    fontSize: "0.85rem",
    background: "#fff",
    color: "#374151",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
  },
  searchStatus: {
    fontSize: "0.85rem",
    color: "#374151",
    margin: "0 0 0.5rem 0",
  },
  error: {
    padding: "0.5rem 0.75rem",
    background: "#fee2e2",
    color: "#991b1b",
    borderRadius: "0.25rem",
    marginBottom: "0.75rem",
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    gap: "0.5rem",
  },
  errorDismiss: {
    background: "transparent",
    border: "none",
    color: "#991b1b",
    fontSize: "1.1rem",
    cursor: "pointer",
    lineHeight: 1,
    padding: "0 0.25rem",
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
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    width: "100%",
    padding: "0.6rem 0.5rem",
    borderBottom: "1px solid #e5e7eb",
    fontSize: "0.9rem",
  },
  emailRowMain: {
    display: "grid",
    gridTemplateColumns: "180px 1fr 120px",
    alignItems: "center",
    gap: "0.75rem",
    flex: 1,
    padding: 0,
    background: "transparent",
    border: "none",
    textAlign: "left",
    cursor: "pointer",
    font: "inherit",
    color: "inherit",
  },
  emailActions: {
    display: "flex",
    gap: "0.25rem",
    flexShrink: 0,
  },
  actionButton: {
    padding: "0.25rem 0.5rem",
    fontSize: "0.75rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
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
