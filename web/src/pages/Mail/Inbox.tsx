import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";

import { jmapClient } from "../../api/jmap";
import { snoozeEmail } from "../../api/snooze";
import { categorize, formatAddresses, getPriorityInbox, type EmailCategory, type PriorityItem } from "../../api/smart";
import { labelsForKeywords, listLabels } from "../../api/labels";
import SnoozePicker from "./SnoozePicker";
import type { Email, Label, Mailbox } from "../../types";

/**
 * Inbox is the primary Mail list view.
 *
 * The page issues JMAP `Mailbox/get` once on mount, then
 * `Email/query` + `Email/get` whenever the selected mailbox
 * changes. Phase 2 push notifications (docs/JMAP-CONTRACT.md §5)
 * are deferred to Phase 3 — state changes come from user
 * navigation for now.
 */
/** WS7: Category tab labels and their API enum values. */
const CATEGORY_TABS: { label: string; value: EmailCategory | "all" }[] = [
  { label: "All", value: "all" },
  { label: "Primary", value: "primary" },
  { label: "Social", value: "social" },
  { label: "Promotions", value: "promotions" },
  { label: "Updates", value: "updates" },
  { label: "Forums", value: "forums" },
];

const CATEGORY_TAB_STYLE: React.CSSProperties = {
  display: "flex",
  gap: 0,
  borderBottom: "2px solid #ddd",
  marginBottom: 8,
  padding: "0 8px",
};

function categoryTabBtn(
  active: boolean,
): React.CSSProperties {
  return {
    padding: "6px 14px",
    cursor: "pointer",
    border: "none",
    background: "none",
    borderBottom: active ? "2px solid #4c8bf5" : "2px solid transparent",
    fontWeight: active ? 600 : 400,
    color: active ? "#4c8bf5" : "#555",
    marginBottom: -2,
  };
}

export default function Inbox() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const isPriorityView = searchParams.get("view") === "priority";
  const { mailboxId: selectedFromRoute } = useParams<{ mailboxId?: string }>();

  const [mailboxes, setMailboxes] = useState<Mailbox[] | null>(null);
  const [emails, setEmails] = useState<Email[] | null>(null);
  const [selectedMailbox, setSelectedMailbox] = useState<string | null>(null);
  // Active label filter (a JMAP keyword) or null. Declared up here
  // because the email-loading effect below reads it to decide whether
  // to query the whole account by keyword or list a single mailbox.
  const [labelFilter, setLabelFilter] = useState<string | null>(null);
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

  // ── WS7: category filter + priority view ──────────────────
  const [activeCategory, setActiveCategory] = useState<EmailCategory | "all">("all");
  const [emailCategories, setEmailCategories] = useState<Record<string, EmailCategory>>({});
  const [priorityItems, setPriorityItems] = useState<PriorityItem[]>([]);
  const [isPriorityLoading, setIsPriorityLoading] = useState(false);

  // Only categorize messages we haven't seen yet, then merge the
  // result into the existing map. This avoids re-fetching the whole
  // window every time the list reloads (e.g. after a mark-read or a
  // category-tab switch), which the previous "categorize all on each
  // change" effect did. `categorizedRef` tracks known ids without
  // adding `emailCategories` to the dependency list (which would loop).
  const categorizedRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (!emails || emails.length === 0) return;
    const unknown = emails
      .map((e) => e.id)
      .filter((id) => !categorizedRef.current.has(id));
    if (unknown.length === 0) return;
    let cancelled = false;
    categorize(unknown)
      .then((res) => {
        if (cancelled) return;
        for (const id of unknown) categorizedRef.current.add(id);
        setEmailCategories((prev) => ({ ...prev, ...res.categories }));
      })
      .catch(() => { /* categorization is best-effort */ });
    return () => { cancelled = true; };
  }, [emails]);

  useEffect(() => {
    if (!isPriorityView) return;
    setIsPriorityLoading(true);
    getPriorityInbox({ limit: 50 })
      .then((res) => setPriorityItems(res.items))
      .catch(() => { /* priority fetch is best-effort */ })
      .finally(() => setIsPriorityLoading(false));
  }, [isPriorityView]);

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
    // A label filter is account-wide: query every mailbox for the
    // keyword (RFC 8621 `hasKeyword`) instead of filtering only the
    // current mailbox's first page, otherwise labelled messages in
    // other mailboxes or beyond the first 50 rows would be invisible.
    const load = labelFilter
      ? jmapClient.getEmailsByKeyword(labelFilter, { limit: 50 })
      : jmapClient.getEmails(selectedMailbox, { limit: 50 });
    load
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
  }, [selectedMailbox, reloadNonce, labelFilter]);

  const handleOpenEmail = useCallback(
    (emailId: string) => {
      // In search mode, or under an account-wide label filter, the
      // row may belong to a different mailbox than the sidebar
      // selection; pick the first mailbox id on the email so the
      // MessageView URL is always valid.
      let mailboxId: string | null = selectedMailbox;
      if (inSearchMode || labelFilter !== null) {
        const list = inSearchMode ? (searchResults ?? []) : (emails ?? []);
        const hit = list.find((e) => e.id === emailId);
        const firstOnEmail = hit ? Object.keys(hit.mailboxIds)[0] : undefined;
        mailboxId = firstOnEmail ?? selectedMailbox;
      }
      if (!mailboxId) return;
      navigate(`/mail/${mailboxId}/${emailId}`);
    },
    [emails, inSearchMode, labelFilter, navigate, searchResults, selectedMailbox],
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
  // `searchResults` converges with server state. The effect is
  // gated on `searchReloadNonce` actually changing — other deps
  // (submittedQuery/submittedScope/selectedMailbox/inSearchMode)
  // are read through refs so changing the sidebar mailbox or
  // submitting a new search does not trigger this refetch (the
  // mailbox effect above and `runSearch` respectively own those
  // code paths).
  const lastProcessedNonceRef = useRef(0);
  const searchRefreshArgsRef = useRef({
    inSearchMode,
    submittedQuery,
    submittedScope,
    selectedMailbox,
  });
  searchRefreshArgsRef.current = {
    inSearchMode,
    submittedQuery,
    submittedScope,
    selectedMailbox,
  };
  useEffect(() => {
    if (searchReloadNonce === 0) return;
    if (lastProcessedNonceRef.current === searchReloadNonce) return;
    lastProcessedNonceRef.current = searchReloadNonce;
    const args = searchRefreshArgsRef.current;
    if (!args.inSearchMode) return;
    let cancelled = false;
    setIsSearching(true);
    jmapClient
      .searchEmails(args.submittedQuery, {
        mailboxId:
          args.submittedScope === "mailbox"
            ? (args.selectedMailbox ?? undefined)
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
  }, [searchReloadNonce]);

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

  const junkMailboxId = useMemo(
    () => (mailboxes ?? []).find((m) => m.role === "junk")?.id ?? null,
    [mailboxes],
  );

  const inboxMailboxId = useMemo(
    () => (mailboxes ?? []).find((m) => m.role === "inbox")?.id ?? null,
    [mailboxes],
  );

  // Priority items are ranked from the inbox window and carry only an
  // email id, so open them against the inbox mailbox (falling back to
  // the current selection) to build a valid MessageView URL.
  const handleOpenPriority = useCallback(
    (emailId: string) => {
      const mailboxId = inboxMailboxId ?? selectedMailbox;
      if (!mailboxId) return;
      navigate(`/mail/${mailboxId}/${emailId}`);
    },
    [inboxMailboxId, navigate, selectedMailbox],
  );

  // Open snooze picker state — one at a time, keyed by email id.
  // Closing == setting to null. The actual handler lives below
  // bumpAfterWrite (state shape here, behaviour after writes).
  const [snoozeOpenFor, setSnoozeOpenFor] = useState<string | null>(null);
  const [snoozeBusy, setSnoozeBusy] = useState(false);

  // Bulk-selection (WS2). Holds the ids of checked rows; the bulk
  // toolbar acts on this set. Cleared whenever the listing changes
  // (mailbox switch, search, refetch) so stale ids never linger.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [bulkBusy, setBulkBusy] = useState(false);
  // Group consecutive messages that share a `threadId` into a single
  // conversation row with a count badge (WS2). Toggleable so users
  // can fall back to the flat list.
  const [groupThreads, setGroupThreads] = useState(true);
  // Client-side label registry. The active label filter itself
  // (`labelFilter`) is declared near the top so the load effect can
  // read it.
  const [labels, setLabels] = useState<Label[]>(() => listLabels());
  // Drag-and-drop: the email id currently being dragged and the drop
  // target (mailbox id or `label:<keyword>`) under the cursor.
  const [dragEmailId, setDragEmailId] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);

  // Clear the bulk selection whenever the underlying listing
  // changes so the toolbar never acts on ids that are no longer
  // visible.
  useEffect(() => {
    setSelectedIds(new Set());
  }, [
    selectedMailbox,
    submittedQuery,
    reloadNonce,
    searchReloadNonce,
    groupThreads,
    labelFilter,
  ]);

  // Labels are edited on a separate page; refresh the registry when
  // the window regains focus so newly-created labels appear without
  // a full reload.
  useEffect(() => {
    const refresh = () => setLabels(listLabels());
    window.addEventListener("focus", refresh);
    return () => window.removeEventListener("focus", refresh);
  }, []);

  // Single source of truth for "this row behaves as if it lives in
  // trash". Used both for the row label (Trash vs Delete) and the
  // handler's delete-vs-move branch so they can't drift. In search
  // mode or under an (account-wide) label filter, results can come
  // from any mailbox so the decision is per-email; otherwise the
  // user is viewing a specific mailbox and the old sidebar-based
  // rule applies (so a message cross-labelled Inbox+Trash still
  // moves when the user clicks Trash from Inbox, matching the
  // pre-search-feature behaviour).
  const isEmailInTrash = useCallback(
    (email: Email): boolean => {
      if (trashMailboxId === null) return false;
      if (inSearchMode || labelFilter !== null) {
        return Object.prototype.hasOwnProperty.call(
          email.mailboxIds,
          trashMailboxId,
        );
      }
      return selectedMailbox === trashMailboxId;
    },
    [inSearchMode, labelFilter, selectedMailbox, trashMailboxId],
  );

  // Mirror of isEmailInTrash for the Junk mailbox. Drives the
  // row-level "Spam" / "Not spam" button label and the
  // handleToggleSpam direction-of-move decision.
  const isEmailInJunk = useCallback(
    (email: Email): boolean => {
      if (junkMailboxId === null) return false;
      if (inSearchMode || labelFilter !== null) {
        return Object.prototype.hasOwnProperty.call(
          email.mailboxIds,
          junkMailboxId,
        );
      }
      return selectedMailbox === junkMailboxId;
    },
    [inSearchMode, junkMailboxId, labelFilter, selectedMailbox],
  );

  // Bump both refetch nonces after a successful write. The
  // mailbox-list effect reads `reloadNonce`; the search effect
  // above reads `searchReloadNonce`. Bumping both here keeps the
  // page converged regardless of which list is currently on screen.
  const bumpAfterWrite = useCallback(() => {
    setReloadNonce((n) => n + 1);
    setSearchReloadNonce((n) => n + 1);
  }, []);

  const handleSnooze = useCallback(
    async (email: Email, until: Date) => {
      if (snoozeBusy) return;
      setSnoozeBusy(true);
      try {
        // Always go through `resolveOrCreateSnoozedMailbox`
        // rather than the local `snoozedMailboxId` memo: the memo
        // reads from React state (`mailboxes`) which may be stale
        // if MessageView just provisioned the mailbox on this
        // session. The shared helper fetches the LIVE list and
        // recovers from concurrent-create races (e.g. two
        // tabs / a re-fetch in flight) by re-looking-up on
        // createMailbox failure.
        const snoozedId = await jmapClient.resolveOrCreateSnoozedMailbox();
        // The user might already have toggled the email into the
        // snoozed mailbox manually; refuse the no-op case so the
        // BFF doesn't reject `snoozed_mailbox_id ∈ originals`.
        const originals = { ...email.mailboxIds } as Record<string, boolean>;
        if (originals[snoozedId]) {
          throw new Error(
            "Email is already in the Snoozed mailbox — wake it first.",
          );
        }
        await snoozeEmail({
          email_id: email.id,
          original_mailbox_ids: originals,
          snoozed_mailbox_id: snoozedId,
          snooze_until: until.toISOString(),
          mark_unread_on_wake: true,
        });
        setSnoozeOpenFor(null);
        bumpAfterWrite();
      } catch (err: unknown) {
        setError(errorMessage(err));
      } finally {
        setSnoozeBusy(false);
      }
    },
    [bumpAfterWrite, snoozeBusy],
  );

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

  const handleToggleSpam = useCallback(
    async (email: Email) => {
      if (!junkMailboxId) {
        setError("Junk mailbox is not available on this account");
        return;
      }
      const inJunk = isEmailInJunk(email);
      // Source mailbox = the non-junk mailbox the email currently
      // lives in (for Inbox → Junk) or the mailbox to move it back
      // to (for Junk → Inbox). In search mode we pick the first
      // mailbox id on the email other than Junk; otherwise fall
      // back to the Inbox role, then to the sidebar selection.
      const emailMailboxIds = Object.keys(email.mailboxIds);
      const nonJunkOnEmail = emailMailboxIds.find(
        (id) => id !== junkMailboxId,
      );
      const counterpart =
        nonJunkOnEmail ?? inboxMailboxId ?? selectedMailbox;
      if (!counterpart) {
        setError("Could not determine source mailbox for this email");
        return;
      }
      try {
        await jmapClient.markAsSpam(
          email.id,
          counterpart,
          junkMailboxId,
          !inJunk,
        );
        bumpAfterWrite();
      } catch (err: unknown) {
        setError(errorMessage(err));
      }
    },
    [
      bumpAfterWrite,
      inboxMailboxId,
      isEmailInJunk,
      junkMailboxId,
      selectedMailbox,
    ],
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
      // mode, or under an account-wide label filter, so the JMAP
      // patch removes it from its actual location rather than a no-op
      // key on the sidebar selection.
      const emailMailboxIds = Object.keys(email.mailboxIds);
      const sourceMailbox =
        inSearchMode || labelFilter !== null
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
    [
      bumpAfterWrite,
      inSearchMode,
      isEmailInTrash,
      labelFilter,
      selectedMailbox,
      trashMailboxId,
    ],
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

  const archiveMailboxId = useMemo(
    () => (mailboxes ?? []).find((m) => m.role === "archive")?.id ?? null,
    [mailboxes],
  );

  // The emails currently in scope (search hits or the mailbox
  // listing), narrowed by the active label filter.
  const baseList = useMemo(
    () => (inSearchMode ? (searchResults ?? []) : (emails ?? [])),
    [inSearchMode, searchResults, emails],
  );
  const filteredList = useMemo(() => {
    const byLabel = labelFilter
      ? baseList.filter((e) => e.keywords[labelFilter] === true)
      : baseList;
    // WS7: narrow to the active category tab. Folded in here (rather
    // than at render time) so thread grouping, selection, and bulk
    // actions all key off the same visible set. Search hits aren't
    // categorized, so the filter is skipped in search mode.
    if (activeCategory !== "all" && !inSearchMode) {
      return byLabel.filter((e) => emailCategories[e.id] === activeCategory);
    }
    return byLabel;
  }, [baseList, labelFilter, activeCategory, emailCategories, inSearchMode]);

  // The rows actually rendered: one per thread (its newest message is
  // the representative `head`) when grouping is on, or one per email
  // otherwise. Selection and "select all" key off the head ids so the
  // header checkbox and per-row checkboxes always agree on the same
  // set. The head is chosen by `receivedAt` rather than first-seen so
  // the representative row is correct regardless of the server's sort
  // direction.
  const displayRows = useMemo(() => {
    if (!groupThreads) {
      return filteredList.map((email) => ({ head: email, emails: [email] }));
    }
    const groups = new Map<string, { head: Email; emails: Email[] }>();
    for (const email of filteredList) {
      const key = email.threadId ?? email.id;
      const existing = groups.get(key);
      if (existing) {
        existing.emails.push(email);
        if ((email.receivedAt ?? "") > (existing.head.receivedAt ?? "")) {
          existing.head = email;
        }
      } else {
        groups.set(key, { head: email, emails: [email] });
      }
    }
    return [...groups.values()];
  }, [filteredList, groupThreads]);

  const displayedIds = useMemo(
    () => displayRows.map((r) => r.head.id),
    [displayRows],
  );

  // Map each row's representative (head) id to every email id it
  // stands for. With conversation grouping on, a row's head id stands
  // for the whole thread, so bulk/drag actions must act on all member
  // messages — not just the newest one shown. Off (or for a single
  // message) the head maps to itself.
  const headToEmailIds = useMemo(() => {
    const map = new Map<string, string[]>();
    for (const row of displayRows) {
      map.set(
        row.head.id,
        row.emails.map((e) => e.id),
      );
    }
    return map;
  }, [displayRows]);

  // Expand selected head ids into the full set of email ids they
  // represent so a bulk action on a collapsed conversation applies to
  // every message in it. Falls back to the id itself for any id not
  // currently rendered as a head.
  const expandSelection = useCallback(
    (ids: string[]): string[] => {
      const out = new Set<string>();
      for (const id of ids) {
        const members = headToEmailIds.get(id);
        if (members) members.forEach((m) => out.add(m));
        else out.add(id);
      }
      return [...out];
    },
    [headToEmailIds],
  );

  const toggleSelected = useCallback((id: string) => {
    setSelectedIds((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const selectAllVisible = useCallback(() => {
    setSelectedIds(new Set(displayedIds));
  }, [displayedIds]);

  const clearSelection = useCallback(() => setSelectedIds(new Set()), []);

  const selectedIdList = useMemo(() => [...selectedIds], [selectedIds]);

  const runBulk = useCallback(
    async (fn: (ids: string[]) => Promise<void>) => {
      // Act on every message the selected rows represent — a collapsed
      // conversation expands to all of its messages, not just the head.
      const ids = expandSelection([...selectedIds]);
      if (ids.length === 0) return;
      setBulkBusy(true);
      try {
        await fn(ids);
        setSelectedIds(new Set());
        bumpAfterWrite();
      } catch (err: unknown) {
        setError(errorMessage(err));
      } finally {
        setBulkBusy(false);
      }
    },
    [bumpAfterWrite, expandSelection, selectedIds],
  );

  // Move many emails to `target`, resolving each email's source
  // mailbox the same way the single-email handlers do. In a plain
  // mailbox listing every row belongs to the sidebar mailbox, so one
  // batched move suffices. In search mode or under an account-wide
  // label filter the selection can span mailboxes, so we group by
  // actual source and issue one move per source — otherwise the
  // `mailboxIds/<selectedMailbox>: null` patch is a no-op on an email
  // that doesn't live there and it gets added to the target without
  // being removed from its real location (RFC 8621 §5.3).
  const bulkMoveResolvingSource = useCallback(
    async (ids: string[], target: string) => {
      if (!inSearchMode && labelFilter === null) {
        await jmapClient.bulkMove(ids, selectedMailbox, target);
        return;
      }
      const bySource = new Map<string | null, string[]>();
      for (const id of ids) {
        const email = baseList.find((e) => e.id === id);
        const source = email ? (Object.keys(email.mailboxIds)[0] ?? null) : null;
        const group = bySource.get(source);
        if (group) group.push(id);
        else bySource.set(source, [id]);
      }
      for (const [source, groupIds] of bySource) {
        await jmapClient.bulkMove(groupIds, source, target);
      }
    },
    [baseList, inSearchMode, labelFilter, selectedMailbox],
  );

  // Bulk equivalent of handleMoveToTrash: emails already in Trash are
  // destroyed, the rest are moved to Trash from their real source.
  const bulkTrash = useCallback(
    async (ids: string[]) => {
      if (!trashMailboxId) return;
      const toDelete: string[] = [];
      const toMove: string[] = [];
      for (const id of ids) {
        const email = baseList.find((e) => e.id === id);
        if (email && isEmailInTrash(email)) toDelete.push(id);
        else toMove.push(id);
      }
      await bulkMoveResolvingSource(toMove, trashMailboxId);
      if (toDelete.length > 0) await jmapClient.bulkDelete(toDelete);
    },
    [baseList, bulkMoveResolvingSource, isEmailInTrash, trashMailboxId],
  );

  // Apply a label (keyword) to one or many emails. Used by the bulk
  // toolbar's label menu and the drag-onto-label flow.
  // Resolves to whether the write succeeded, so callers can keep the
  // selection intact on failure (lets the user retry) instead of
  // clearing it unconditionally.
  const applyLabelTo = useCallback(
    async (keyword: string, ids: string[]): Promise<boolean> => {
      if (ids.length === 0) return false;
      setBulkBusy(true);
      try {
        await jmapClient.bulkSetKeyword(ids, keyword, true);
        bumpAfterWrite();
        return true;
      } catch (err: unknown) {
        setError(errorMessage(err));
        return false;
      } finally {
        setBulkBusy(false);
      }
    },
    [bumpAfterWrite],
  );

  // Drop an email onto a sidebar target. A mailbox id moves the
  // email there; a `label:<keyword>` target applies that label.
  const handleDropOnTarget = useCallback(
    (target: string) => {
      const emailId = dragEmailId;
      setDragEmailId(null);
      setDropTarget(null);
      if (!emailId) return;
      // A dragged row may be a collapsed conversation; act on every
      // message it represents, mirroring the bulk toolbar.
      const ids = expandSelection([emailId]);
      if (target.startsWith("label:")) {
        void applyLabelTo(target.slice("label:".length), ids);
        return;
      }
      // Skip only when *every* message the dragged row represents
      // already lives in the target; otherwise move it. Gating on the
      // head message alone would silently skip the move for a collapsed
      // conversation whose newest message is already in the target but
      // whose older messages are not. bulkMoveResolvingSource resolves
      // each message's real source, so a conversation spanning mailboxes
      // is fully moved.
      const allInTarget = ids.every((id) => {
        const e = baseList.find((x) => x.id === id);
        return (
          !!e && Object.prototype.hasOwnProperty.call(e.mailboxIds, target)
        );
      });
      if (allInTarget) return;
      setBulkBusy(true);
      bulkMoveResolvingSource(ids, target)
        .then(() => bumpAfterWrite())
        .catch((err: unknown) => setError(errorMessage(err)))
        .finally(() => setBulkBusy(false));
    },
    [
      applyLabelTo,
      baseList,
      bulkMoveResolvingSource,
      bumpAfterWrite,
      dragEmailId,
      expandSelection,
    ],
  );

  const selectedCount = selectedIds.size;

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
              const isJunk = mb.role === "junk";
              return (
                <li key={mb.id}>
                  <button
                    type="button"
                    onClick={() => setSelectedMailbox(mb.id)}
                    onDragOver={(e) => {
                      if (dragEmailId) {
                        e.preventDefault();
                        setDropTarget(mb.id);
                      }
                    }}
                    onDragLeave={() =>
                      setDropTarget((t) => (t === mb.id ? null : t))
                    }
                    onDrop={(e) => {
                      e.preventDefault();
                      handleDropOnTarget(mb.id);
                    }}
                    style={{
                      ...layoutStyles.mailboxItem,
                      ...(isSelected ? layoutStyles.mailboxItemActive : {}),
                      ...(isJunk ? layoutStyles.mailboxItemJunk : {}),
                      ...(dropTarget === mb.id
                        ? layoutStyles.dropTargetActive
                        : {}),
                    }}
                    title={isJunk ? "Spam / junk mail" : mb.name}
                  >
                    <span>
                      {isJunk && (
                        <span aria-hidden="true" style={layoutStyles.junkIcon}>
                          ⚠
                        </span>
                      )}
                      {mb.name}
                    </span>
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
        <div style={layoutStyles.labelSection}>
          <div style={layoutStyles.labelSectionHeader}>
            <span style={layoutStyles.labelSectionTitle}>Labels</span>
            <Link to="/mail/labels" style={layoutStyles.labelManageLink}>
              Manage
            </Link>
          </div>
          {labels.length === 0 ? (
            <p style={layoutStyles.muted}>No labels yet.</p>
          ) : (
            <ul style={layoutStyles.mailboxList}>
              {labelFilter && (
                <li>
                  <button
                    type="button"
                    onClick={() => setLabelFilter(null)}
                    style={layoutStyles.labelClear}
                  >
                    Clear label filter
                  </button>
                </li>
              )}
              {labels.map((label) => {
                const target = `label:${label.keyword}`;
                const active = labelFilter === label.keyword;
                return (
                  <li key={label.id}>
                    <button
                      type="button"
                      onClick={() =>
                        setLabelFilter((cur) =>
                          cur === label.keyword ? null : label.keyword,
                        )
                      }
                      onDragOver={(e) => {
                        if (dragEmailId) {
                          e.preventDefault();
                          setDropTarget(target);
                        }
                      }}
                      onDragLeave={() =>
                        setDropTarget((t) => (t === target ? null : t))
                      }
                      onDrop={(e) => {
                        e.preventDefault();
                        handleDropOnTarget(target);
                      }}
                      style={{
                        ...layoutStyles.mailboxItem,
                        ...(active ? layoutStyles.mailboxItemActive : {}),
                        ...(dropTarget === target
                          ? layoutStyles.dropTargetActive
                          : {}),
                      }}
                      title={`Filter by ${label.name} (drag an email here to apply)`}
                    >
                      <span style={layoutStyles.labelNameWrap}>
                        <span
                          aria-hidden="true"
                          style={{
                            ...layoutStyles.labelDot,
                            background: label.color,
                          }}
                        />
                        {label.name}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
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
        {/* WS7: category tabs — shown when viewing the normal inbox. */}
        {!inSearchMode && !isPriorityView && (
          <div style={CATEGORY_TAB_STYLE}>
            {CATEGORY_TABS.map((tab) => (
              <button
                key={tab.value}
                type="button"
                style={categoryTabBtn(activeCategory === tab.value)}
                onClick={() => setActiveCategory(tab.value)}
              >
                {tab.label}
              </button>
            ))}
          </div>
        )}
        {/* WS7: priority view replaces the standard list when active. */}
        {isPriorityView && (
          <div>
            <h3 style={{ margin: "0 0 8px" }}>Priority Inbox</h3>
            {isPriorityLoading && <p style={{ color: "#888" }}>Loading…</p>}
            {!isPriorityLoading && priorityItems.length === 0 && (
              <p style={{ color: "#888" }}>No priority messages.</p>
            )}
            {!isPriorityLoading && priorityItems.length > 0 && (
              <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
                {priorityItems.map((item) => (
                  <li
                    key={item.email_id}
                    role="button"
                    tabIndex={0}
                    onClick={() => handleOpenPriority(item.email_id)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        handleOpenPriority(item.email_id);
                      }
                    }}
                    style={{
                      padding: "8px 12px",
                      borderBottom: "1px solid #eee",
                      cursor: "pointer",
                    }}
                  >
                    <div style={{ fontWeight: 500 }}>
                      {item.subject || "(no subject)"}
                    </div>
                    <div style={{ fontSize: "0.85rem", color: "#666" }}>
                      {formatAddresses(item.from)} — score {item.score}
                    </div>
                    <div style={{ fontSize: "0.8rem", color: "#999" }}>
                      {item.preview?.slice(0, 120)}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
        {!isPriorityView && filteredList.length > 0 && (
          <div style={layoutStyles.listControls}>
            <label style={layoutStyles.controlToggle}>
              <input
                type="checkbox"
                checked={
                  selectedCount > 0 && selectedCount === displayedIds.length
                }
                ref={(el) => {
                  if (el)
                    el.indeterminate =
                      selectedCount > 0 &&
                      selectedCount < displayedIds.length;
                }}
                onChange={(e) =>
                  e.target.checked ? selectAllVisible() : clearSelection()
                }
                aria-label="Select all messages"
              />
              {selectedCount > 0 ? `${selectedCount} selected` : "Select all"}
            </label>
            <label style={layoutStyles.controlToggle}>
              <input
                type="checkbox"
                checked={groupThreads}
                onChange={(e) => setGroupThreads(e.target.checked)}
              />
              Group by conversation
            </label>
            {labelFilter && (
              <span style={layoutStyles.filterPill}>
                {labelsForKeywords({ [labelFilter]: true })[0]?.name ??
                  "Label"}
                <button
                  type="button"
                  onClick={() => setLabelFilter(null)}
                  style={layoutStyles.filterPillClear}
                  aria-label="Clear label filter"
                >
                  ×
                </button>
              </span>
            )}
          </div>
        )}
        {!isPriorityView && selectedCount > 0 && (
          <div style={layoutStyles.bulkBar}>
            <span style={layoutStyles.bulkCount}>{selectedCount} selected</span>
            <button
              type="button"
              disabled={bulkBusy}
              onClick={() =>
                void runBulk((ids) => jmapClient.bulkSetSeen(ids, true))
              }
              style={layoutStyles.bulkButton}
            >
              Mark read
            </button>
            <button
              type="button"
              disabled={bulkBusy}
              onClick={() =>
                void runBulk((ids) => jmapClient.bulkSetSeen(ids, false))
              }
              style={layoutStyles.bulkButton}
            >
              Mark unread
            </button>
            {archiveMailboxId && (
              <button
                type="button"
                disabled={bulkBusy}
                onClick={() =>
                  void runBulk((ids) =>
                    bulkMoveResolvingSource(ids, archiveMailboxId),
                  )
                }
                style={layoutStyles.bulkButton}
              >
                Archive
              </button>
            )}
            {trashMailboxId && (
              <button
                type="button"
                disabled={bulkBusy}
                onClick={() => void runBulk((ids) => bulkTrash(ids))}
                style={layoutStyles.bulkButton}
              >
                {inTrashView ? "Delete" : "Trash"}
              </button>
            )}
            {labels.length > 0 && (
              <select
                disabled={bulkBusy}
                value=""
                onChange={(e) => {
                  const kw = e.target.value;
                  if (!kw) return;
                  void applyLabelTo(kw, expandSelection(selectedIdList)).then(
                    (ok) => {
                      if (ok) setSelectedIds(new Set());
                    },
                  );
                  e.target.value = "";
                }}
                style={layoutStyles.bulkSelect}
                aria-label="Apply label to selected"
              >
                <option value="">Apply label…</option>
                {labels.map((l) => (
                  <option key={l.id} value={l.keyword}>
                    {l.name}
                  </option>
                ))}
              </select>
            )}
            <button
              type="button"
              onClick={clearSelection}
              style={layoutStyles.bulkButton}
            >
              Clear
            </button>
          </div>
        )}
        {!isPriorityView && filteredList.length > 0 && (
          <ul style={layoutStyles.emailList} data-testid="email-list">
            {displayRows.map(({ head, emails: groupEmails }) => {
              const email = head;
              // Reuse the single-source-of-truth helpers (which now
              // also treat an active label filter as account-wide) so
              // the row label and the move handler can't disagree.
              const rowInTrash = isEmailInTrash(email);
              const rowInJunk = isEmailInJunk(email);
              const threadCount = groupEmails.length;
              return (
                <EmailRow
                  key={groupThreads ? (email.threadId ?? email.id) : email.id}
                  email={email}
                  threadCount={threadCount}
                  rowLabels={labelsForKeywords(email.keywords)}
                  selected={selectedIds.has(email.id)}
                  onToggleSelected={() => toggleSelected(email.id)}
                  onDragStart={() => setDragEmailId(email.id)}
                  onDragEnd={() => {
                    setDragEmailId(null);
                    setDropTarget(null);
                  }}
                  inTrashView={rowInTrash}
                  inJunkView={rowInJunk}
                  hasJunkMailbox={junkMailboxId !== null}
                  snoozeOpen={snoozeOpenFor === email.id}
                  snoozeBusy={snoozeBusy && snoozeOpenFor === email.id}
                  onOpen={() =>
                    threadCount > 1 && email.threadId
                      ? navigate(`/mail/thread/${email.threadId}`)
                      : handleOpenEmail(email.id)
                  }
                  onToggleRead={() => handleToggleRead(email)}
                  onMoveToTrash={() => handleMoveToTrash(email)}
                  onToggleSpam={() => handleToggleSpam(email)}
                  onOpenSnooze={() => setSnoozeOpenFor(email.id)}
                  onCancelSnooze={() => setSnoozeOpenFor(null)}
                  onPickSnooze={(until) => handleSnooze(email, until)}
                />
              );
            })}
          </ul>
        )}
      </main>
    </section>
  );
}

interface EmailRowProps {
  email: Email;
  threadCount: number;
  rowLabels: Label[];
  selected: boolean;
  onToggleSelected: () => void;
  onDragStart: () => void;
  onDragEnd: () => void;
  inTrashView: boolean;
  inJunkView: boolean;
  hasJunkMailbox: boolean;
  snoozeOpen: boolean;
  snoozeBusy: boolean;
  onOpen: () => void;
  onToggleRead: () => void;
  onMoveToTrash: () => void;
  onToggleSpam: () => void;
  onOpenSnooze: () => void;
  onCancelSnooze: () => void;
  onPickSnooze: (until: Date) => void;
}

function EmailRow({
  email,
  threadCount,
  rowLabels,
  selected,
  onToggleSelected,
  onDragStart,
  onDragEnd,
  inTrashView,
  inJunkView,
  hasJunkMailbox,
  snoozeOpen,
  snoozeBusy,
  onOpen,
  onToggleRead,
  onMoveToTrash,
  onToggleSpam,
  onOpenSnooze,
  onCancelSnooze,
  onPickSnooze,
}: EmailRowProps) {
  const isUnread = !email.keywords.$seen;
  const from = email.from?.[0];
  const sender = from?.name ?? from?.email ?? "(unknown sender)";
  const subject = email.subject ?? "(no subject)";
  const dateLabel = formatDate(email.receivedAt);
  return (
    <li>
      <div
        draggable
        onDragStart={(e) => {
          e.dataTransfer.effectAllowed = "move";
          e.dataTransfer.setData("text/plain", email.id);
          onDragStart();
        }}
        onDragEnd={onDragEnd}
        style={{
          ...layoutStyles.emailRow,
          ...(isUnread ? layoutStyles.emailRowUnread : {}),
          ...(inJunkView ? layoutStyles.emailRowJunk : {}),
          ...(selected ? layoutStyles.emailRowSelected : {}),
        }}
      >
        <input
          type="checkbox"
          checked={selected}
          onClick={(e) => e.stopPropagation()}
          onChange={onToggleSelected}
          style={layoutStyles.rowCheckbox}
          aria-label={`Select message from ${sender}`}
        />
        {inJunkView && (
          <span
            style={layoutStyles.junkRowBadge}
            title="Filed as spam by the server or by a user"
            aria-label="Junk"
          >
            SPAM
          </span>
        )}
        <button
          type="button"
          onClick={onOpen}
          style={layoutStyles.emailRowMain}
        >
          <span style={layoutStyles.emailSender}>
            {sender}
            {threadCount > 1 && (
              <span style={layoutStyles.threadBadge} title={`${threadCount} messages in this conversation`}>
                {threadCount}
              </span>
            )}
          </span>
          <span style={layoutStyles.emailSubjectWrap}>
            <span style={layoutStyles.emailSubject}>{subject}</span>
            {rowLabels.map((l) => (
              <span
                key={l.id}
                style={{ ...layoutStyles.rowLabelChip, background: l.color }}
              >
                {l.name}
              </span>
            ))}
          </span>
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
          {hasJunkMailbox && (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onToggleSpam();
              }}
              style={layoutStyles.actionButton}
              title={
                inJunkView
                  ? "Not spam — move back to Inbox and clear the junk flag"
                  : "Mark as spam — move to Junk and train the spam classifier"
              }
            >
              {inJunkView ? "Not spam" : "Spam"}
            </button>
          )}
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
          <div style={layoutStyles.snoozeWrap}>
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onOpenSnooze();
              }}
              disabled={snoozeBusy}
              style={layoutStyles.actionButton}
              title="Snooze this email until later"
              aria-haspopup="dialog"
              aria-expanded={snoozeOpen}
            >
              {snoozeBusy ? "Snoozing…" : "Snooze"}
            </button>
            {snoozeOpen && (
              <SnoozePicker
                onPick={(until) => onPickSnooze(until)}
                onCancel={onCancelSnooze}
              />
            )}
          </div>
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
  mailboxItemJunk: {
    color: "#92400e",
  },
  junkIcon: {
    marginRight: "0.35rem",
    color: "#d97706",
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
  snoozeWrap: {
    position: "relative",
    display: "inline-block",
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
  emailRowJunk: {
    background: "#fef3c7",
  },
  junkRowBadge: {
    display: "inline-flex",
    alignItems: "center",
    padding: "0.1rem 0.4rem",
    fontSize: "0.65rem",
    fontWeight: 700,
    letterSpacing: "0.05em",
    background: "#d97706",
    color: "#fff",
    borderRadius: "0.25rem",
    flexShrink: 0,
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
  dropTargetActive: {
    outline: "2px dashed #2563eb",
    background: "#eff6ff",
  },
  labelSection: {
    marginTop: "1rem",
    paddingTop: "0.75rem",
    borderTop: "1px solid #e5e7eb",
  },
  labelSectionHeader: {
    display: "flex",
    alignItems: "center",
    justifyContent: "space-between",
    marginBottom: "0.5rem",
  },
  labelSectionTitle: {
    fontSize: "0.75rem",
    fontWeight: 700,
    textTransform: "uppercase",
    letterSpacing: "0.05em",
    color: "#6b7280",
  },
  labelManageLink: {
    fontSize: "0.75rem",
    color: "#2563eb",
    textDecoration: "none",
  },
  labelClear: {
    width: "100%",
    padding: "0.25rem 0.5rem",
    fontSize: "0.78rem",
    background: "transparent",
    border: "none",
    textAlign: "left",
    cursor: "pointer",
    color: "#6b7280",
  },
  labelNameWrap: {
    display: "inline-flex",
    alignItems: "center",
    gap: "0.4rem",
  },
  labelDot: {
    width: "0.7rem",
    height: "0.7rem",
    borderRadius: "999px",
    flexShrink: 0,
  },
  listControls: {
    display: "flex",
    alignItems: "center",
    gap: "1rem",
    flexWrap: "wrap",
    padding: "0.25rem 0",
    marginBottom: "0.25rem",
  },
  controlToggle: {
    display: "inline-flex",
    alignItems: "center",
    gap: "0.35rem",
    fontSize: "0.82rem",
    color: "#374151",
    cursor: "pointer",
  },
  filterPill: {
    display: "inline-flex",
    alignItems: "center",
    gap: "0.35rem",
    fontSize: "0.78rem",
    background: "#e0e7ff",
    color: "#3730a3",
    padding: "0.1rem 0.5rem",
    borderRadius: "999px",
  },
  filterPillClear: {
    border: "none",
    background: "none",
    color: "#3730a3",
    cursor: "pointer",
    fontSize: "0.9rem",
    lineHeight: 1,
  },
  bulkBar: {
    display: "flex",
    alignItems: "center",
    gap: "0.5rem",
    flexWrap: "wrap",
    padding: "0.5rem 0.6rem",
    background: "#f3f4f6",
    border: "1px solid #e5e7eb",
    borderRadius: "0.25rem",
    marginBottom: "0.5rem",
  },
  bulkCount: {
    fontSize: "0.82rem",
    fontWeight: 600,
    color: "#374151",
  },
  bulkButton: {
    padding: "0.3rem 0.6rem",
    fontSize: "0.78rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
    cursor: "pointer",
    color: "#374151",
  },
  bulkSelect: {
    padding: "0.3rem 0.5rem",
    fontSize: "0.78rem",
    background: "#fff",
    border: "1px solid #d1d5db",
    borderRadius: "0.25rem",
  },
  emailRowSelected: {
    background: "#eef2ff",
  },
  rowCheckbox: {
    flexShrink: 0,
    cursor: "pointer",
  },
  threadBadge: {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    minWidth: "1.1rem",
    height: "1.1rem",
    marginLeft: "0.4rem",
    padding: "0 0.3rem",
    fontSize: "0.7rem",
    fontWeight: 700,
    background: "#9ca3af",
    color: "#fff",
    borderRadius: "999px",
  },
  emailSubjectWrap: {
    display: "flex",
    alignItems: "center",
    gap: "0.4rem",
    overflow: "hidden",
  },
  rowLabelChip: {
    flexShrink: 0,
    fontSize: "0.65rem",
    fontWeight: 600,
    color: "#fff",
    padding: "0.05rem 0.4rem",
    borderRadius: "999px",
    whiteSpace: "nowrap",
  },
};
