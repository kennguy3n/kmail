import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { InboxIcon, Mail, Search, Tag, Trash2, Clock, Eye, EyeOff, AlertTriangle } from "lucide-react";

import { cn } from "../../lib/cn";
import { Avatar } from "../../components/ui/Avatar";
import { Button } from "../../components/ui/Button";
import { EmptyState } from "../../components/ui/EmptyState";

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

const CATEGORY_TAB_STYLE = "flex gap-1 border-b border-border px-2 pb-0";

function categoryTabBtn(active: boolean): string {
  return cn(
    "relative -mb-px cursor-pointer rounded-t-lg border-0 bg-transparent px-4 py-2 text-sm font-medium transition-colors",
    active
      ? "text-primary after:absolute after:bottom-0 after:left-2 after:right-2 after:h-0.5 after:rounded-full after:bg-primary"
      : "text-fg-muted hover:bg-surface-hover hover:text-fg",
  );
}

export default function Inbox() {
  const navigate = useNavigate();
  // Priority Inbox is its own route (`/mail/priority`) rather than a
  // `?view=priority` query param so the WS1 sidebar/breadcrumb model —
  // which matches on `pathname` only — highlights the active link and
  // builds the right crumb trail.
  const { pathname } = useLocation();
  const isPriorityView = pathname === "/mail/priority";
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
    <section className={layoutStyles.root}>
      <aside className={layoutStyles.sidebar}>
        <div className={layoutStyles.sidebarHeader}>
          <h2 className={layoutStyles.sidebarTitle}>Mail</h2>
          <Link to="/mail/compose" className={layoutStyles.composeButton}>
            Compose
          </Link>
        </div>
        {isLoadingMailboxes ? (
          <p className={layoutStyles.muted}>Loading mailboxes…</p>
        ) : (
          <ul className={layoutStyles.mailboxList}>
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
                    className={cn(
                      layoutStyles.mailboxItem,
                      isSelected && layoutStyles.mailboxItemActive,
                      isJunk && layoutStyles.mailboxItemJunk,
                      dropTarget === mb.id && layoutStyles.dropTargetActive,
                    )}
                    title={isJunk ? "Spam / junk mail" : mb.name}
                  >
                    <span>
                      {isJunk && (
                        <span aria-hidden="true" className={layoutStyles.junkIcon}>
                          ⚠
                        </span>
                      )}
                      {mb.name}
                    </span>
                    {mb.unreadEmails > 0 && (
                      <span className={layoutStyles.unreadBadge}>
                        {mb.unreadEmails}
                      </span>
                    )}
                  </button>
                </li>
              );
            })}
          </ul>
        )}
        <div className={layoutStyles.labelSection}>
          <div className={layoutStyles.labelSectionHeader}>
            <span className={layoutStyles.labelSectionTitle}>Labels</span>
            <Link to="/mail/labels" className={layoutStyles.labelManageLink}>
              Manage
            </Link>
          </div>
          {labels.length === 0 ? (
            <EmptyState
              icon={<Tag />}
              title="No labels yet"
              description="Create labels to organize your messages."
            />
          ) : (
            <ul className={layoutStyles.mailboxList}>
              {labelFilter && (
                <li>
                  <button
                    type="button"
                    onClick={() => setLabelFilter(null)}
                    className={layoutStyles.labelClear}
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
                      className={cn(
                        layoutStyles.mailboxItem,
                        active && layoutStyles.mailboxItemActive,
                        dropTarget === target && layoutStyles.dropTargetActive,
                      )}
                      title={`Filter by ${label.name} (drag an email here to apply)`}
                    >
                      <span className={layoutStyles.labelNameWrap}>
                        <span
                          aria-hidden="true"
                          className={layoutStyles.labelDot}
                          style={{ background: label.color }}
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
      <main className={layoutStyles.main}>
        <form className={layoutStyles.searchBar} onSubmit={handleSubmitSearch}>
          <input
            type="search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search mail…"
            aria-label="Search mail"
            className={layoutStyles.searchInput}
          />
          <label className={layoutStyles.searchScopeLabel}>
            <input
              type="checkbox"
              checked={searchScope === "global"}
              onChange={(e) =>
                setSearchScope(e.target.checked ? "global" : "mailbox")
              }
            />
            All mailboxes
          </label>
          <button type="submit" className={layoutStyles.searchButton}>
            Search
          </button>
          {inSearchMode && (
            <button
              type="button"
              onClick={handleClearSearch}
              className={layoutStyles.searchClear}
            >
              Clear
            </button>
          )}
        </form>
        {error && (
          <div className={layoutStyles.error}>
            <span>{error}</span>
            <button
              type="button"
              onClick={() => setError(null)}
              className={layoutStyles.errorDismiss}
              aria-label="Dismiss error"
            >
              ×
            </button>
          </div>
        )}
        {inSearchMode && (
          <p className={layoutStyles.searchStatus}>
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
          <EmptyState
            icon={<Mail />}
            title="Loading emails…"
            description="Please wait while we fetch your messages."
          />
        )}
        {!inSearchMode &&
          !isLoadingEmails &&
          emails &&
          emails.length === 0 && (
            <EmptyState
              icon={<InboxIcon />}
              title="Inbox is empty"
              description="You're all caught up. New messages will appear here."
              action={
                <Button
                  iconLeft={<Mail />}
                  onClick={() => navigate("/mail/compose")}
                >
                  Compose
                </Button>
              }
            />
          )}
        {inSearchMode &&
          !isSearching &&
          searchResults &&
          searchResults.length === 0 && (
            <EmptyState
              icon={<Search />}
              title="No matching messages"
              description={`We couldn't find anything for “${submittedQuery}”. Try a different term or search across all mail.`}
            />
          )}
        {/* WS7: category tabs — shown when viewing the normal inbox. */}
        {!inSearchMode && !isPriorityView && (
          <div className={CATEGORY_TAB_STYLE}>
            {CATEGORY_TABS.map((tab) => (
              <button
                key={tab.value}
                type="button"
                className={categoryTabBtn(activeCategory === tab.value)}
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
            <h3 className="mb-2 mt-0 text-base font-semibold">Priority Inbox</h3>
            {isPriorityLoading && (
              <EmptyState
                icon={<Mail />}
                title="Loading priority messages…"
              />
            )}
            {!isPriorityLoading && priorityItems.length === 0 && (
              <EmptyState
                icon={<InboxIcon />}
                title="No priority messages"
                description="Important messages will surface here once they arrive."
              />
            )}
            {!isPriorityLoading && priorityItems.length > 0 && (
              <ul className="m-0 list-none p-0">
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
                    className="cursor-pointer border-b border-border px-3 py-2 hover:bg-surface-hover"
                  >
                    <div className="font-medium">
                      {item.subject || "(no subject)"}
                    </div>
                    <div className="text-sm text-fg-muted">
                      {formatAddresses(item.from)} — score {item.score}
                    </div>
                    <div className="text-xs text-fg-subtle">
                      {item.preview?.slice(0, 120)}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
        {!isPriorityView && filteredList.length > 0 && (
          <div className={layoutStyles.listControls}>
            <label className={layoutStyles.controlToggle}>
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
            <label className={layoutStyles.controlToggle}>
              <input
                type="checkbox"
                checked={groupThreads}
                onChange={(e) => setGroupThreads(e.target.checked)}
              />
              Group by conversation
            </label>
            {labelFilter && (
              <span className={layoutStyles.filterPill}>
                {labelsForKeywords({ [labelFilter]: true })[0]?.name ??
                  "Label"}
                <button
                  type="button"
                  onClick={() => setLabelFilter(null)}
                  className={layoutStyles.filterPillClear}
                  aria-label="Clear label filter"
                >
                  ×
                </button>
              </span>
            )}
          </div>
        )}
        {!isPriorityView && selectedCount > 0 && (
          <div className={layoutStyles.bulkBar}>
            <span className={layoutStyles.bulkCount}>{selectedCount} selected</span>
            <button
              type="button"
              disabled={bulkBusy}
              onClick={() =>
                void runBulk((ids) => jmapClient.bulkSetSeen(ids, true))
              }
              className={layoutStyles.bulkButton}
            >
              Mark read
            </button>
            <button
              type="button"
              disabled={bulkBusy}
              onClick={() =>
                void runBulk((ids) => jmapClient.bulkSetSeen(ids, false))
              }
              className={layoutStyles.bulkButton}
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
                className={layoutStyles.bulkButton}
              >
                Archive
              </button>
            )}
            {trashMailboxId && (
              <button
                type="button"
                disabled={bulkBusy}
                onClick={() => void runBulk((ids) => bulkTrash(ids))}
                className={layoutStyles.bulkButton}
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
                className={layoutStyles.bulkSelect}
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
              className={layoutStyles.bulkButton}
            >
              Clear
            </button>
          </div>
        )}
        {!isPriorityView && filteredList.length > 0 && (
          <ul className={layoutStyles.emailList} data-testid="email-list">
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
  const preview = email.preview ?? "";
  const dateLabel = formatDate(email.receivedAt);
  return (
    <li className="group relative">
      <div
        draggable
        onDragStart={(e) => {
          e.dataTransfer.effectAllowed = "move";
          e.dataTransfer.setData("text/plain", email.id);
          onDragStart();
        }}
        onDragEnd={onDragEnd}
        className={cn(
          "flex cursor-pointer items-center gap-3 border-b border-border px-3 py-2.5 transition-colors hover:bg-surface-hover",
          isUnread && "bg-surface",
          selected && "bg-surface-active",
        )}
      >
        <input
          type="checkbox"
          checked={selected}
          onClick={(e) => e.stopPropagation()}
          onChange={onToggleSelected}
          className="shrink-0 cursor-pointer"
          aria-label={`Select message from ${sender}`}
        />
        {inJunkView && (
          <span
            className="inline-flex shrink-0 items-center rounded-md bg-warning px-1.5 py-0.5 text-[0.65rem] font-bold tracking-wider text-white"
            title="Filed as spam by the server or by a user"
            aria-label="Junk"
          >
            SPAM
          </span>
        )}
        <button
          type="button"
          onClick={onOpen}
          className="flex flex-1 items-center gap-3 overflow-hidden text-left"
        >
          <Avatar name={sender} size="sm" />
          <span className="w-36 shrink-0 truncate text-sm font-medium text-fg">
            {sender}
          </span>
          <span className="min-w-0 flex-1 truncate">
            <span className={cn("text-sm", isUnread ? "font-semibold text-fg" : "text-fg")}>
              {subject}
            </span>
            {preview && (
              <span className="text-sm text-fg-muted">
                {" — "}
                {preview}
              </span>
            )}
            {rowLabels.length > 0 && (
              <span className="ml-2 inline-flex gap-1">
                {rowLabels.map((l) => (
                  <span
                    key={l.id}
                    className="inline-flex items-center rounded-pill px-2 py-0.5 text-[0.65rem] font-semibold text-white"
                    style={{ background: l.color }}
                  >
                    {l.name}
                  </span>
                ))}
              </span>
            )}
            {threadCount > 1 && (
              <span
                className="ml-2 inline-flex h-[1.1rem] min-w-[1.1rem] items-center justify-center rounded-pill bg-fg-subtle px-1 text-[0.7rem] font-bold text-white"
                title={`${threadCount} messages in this conversation`}
              >
                {threadCount}
              </span>
            )}
          </span>
          <span className="shrink-0 text-xs text-fg-muted">{dateLabel}</span>
        </button>
        <div className="flex shrink-0 items-center gap-1 opacity-0 transition-opacity group-hover:opacity-100">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onToggleRead();
            }}
            className="inline-flex size-8 items-center justify-center rounded-md text-fg-muted hover:bg-surface-hover hover:text-fg"
            title={isUnread ? "Mark as read" : "Mark as unread"}
            aria-label={isUnread ? "Mark as read" : "Mark as unread"}
          >
            {isUnread ? <Eye className="size-4" /> : <EyeOff className="size-4" />}
          </button>
          {hasJunkMailbox && (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onToggleSpam();
              }}
              className="inline-flex size-8 items-center justify-center rounded-md text-fg-muted hover:bg-warning-bg hover:text-warning"
              title={
                inJunkView
                  ? "Not spam — move back to Inbox"
                  : "Mark as spam"
              }
              aria-label={inJunkView ? "Not spam" : "Mark as spam"}
            >
              <AlertTriangle className="size-4" />
            </button>
          )}
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onMoveToTrash();
            }}
            className="inline-flex size-8 items-center justify-center rounded-md text-fg-muted hover:bg-danger-bg hover:text-danger"
            title={inTrashView ? "Delete permanently" : "Move to trash"}
            aria-label={inTrashView ? "Delete permanently" : "Move to trash"}
          >
            <Trash2 className="size-4" />
          </button>
          <div className="relative inline-block">
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onOpenSnooze();
              }}
              disabled={snoozeBusy}
              className="inline-flex size-8 items-center justify-center rounded-md text-fg-muted hover:bg-surface-hover hover:text-fg disabled:opacity-50"
              title="Snooze"
              aria-haspopup="dialog"
              aria-expanded={snoozeOpen}
              aria-label="Snooze"
            >
              <Clock className="size-4" />
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


/**
 * Tailwind class recipes for the Inbox shell. Values resolve to the
 * semantic design tokens (via `tailwind.config.ts`) so the list,
 * sidebar and row chrome flip automatically with the active theme
 * instead of being pinned to the old hard-coded hex palette.
 */
const layoutStyles: Record<string, string> = {
  root: "grid grid-cols-[220px_1fr] gap-4 min-h-[calc(100vh-4rem)]",
  sidebar: "border-r border-border bg-surface-muted p-4",
  sidebarHeader: "mb-3 flex items-center justify-between",
  sidebarTitle: "m-0 text-lg font-semibold",
  composeButton:
    "rounded-md bg-primary px-2 py-1 text-sm text-primary-fg no-underline transition-colors hover:bg-primary-hover hover:no-underline",
  mailboxList: "m-0 flex list-none flex-col gap-0.5 p-0",
  mailboxItem:
    "flex w-full cursor-pointer items-center justify-between rounded-md border-0 bg-transparent px-2 py-1.5 text-left text-sm text-fg transition-colors hover:bg-surface-hover",
  mailboxItemActive: "bg-primary-subtle font-semibold text-primary",
  mailboxItemJunk: "text-warning-fg",
  junkIcon: "mr-1.5 text-warning",
  unreadBadge:
    "rounded-pill bg-primary px-1.5 py-0.5 text-xs text-primary-fg",
  main: "p-4",
  snoozeWrap: "relative inline-block",
  searchBar: "mb-3 flex items-center gap-2",
  searchInput:
    "flex-1 rounded-md border border-border bg-surface px-2.5 py-1.5 text-sm text-fg outline-none focus-visible:border-primary focus-visible:ring-2 focus-visible:ring-primary-subtle",
  searchScopeLabel: "flex items-center gap-1 text-xs text-fg-muted",
  searchButton:
    "cursor-pointer rounded-md border-0 bg-primary px-3 py-1.5 text-sm text-primary-fg transition-colors hover:bg-primary-hover",
  searchClear:
    "cursor-pointer rounded-md border border-border bg-surface px-3 py-1.5 text-sm text-fg transition-colors hover:bg-surface-hover",
  searchStatus: "mb-2 text-sm text-fg-muted",
  error:
    "mb-3 flex items-center justify-between gap-2 rounded-md bg-danger-bg px-3 py-2 text-danger-fg",
  errorDismiss:
    "cursor-pointer border-0 bg-transparent px-1 text-lg leading-none text-danger-fg",
  muted: "italic text-fg-muted",
  emailList: "m-0 list-none border-t border-border p-0",
  emailRow:
    "flex w-full items-center gap-2 border-b border-border px-2 py-2.5 text-sm",
  emailRowMain:
    "grid flex-1 cursor-pointer grid-cols-[180px_1fr_120px] items-center gap-3 border-0 bg-transparent p-0 text-left font-[inherit] text-inherit",
  emailActions: "flex shrink-0 gap-1",
  actionButton:
    "cursor-pointer rounded-md border border-border bg-surface px-2 py-1 text-xs text-fg transition-colors hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-60",
  emailRowUnread: "bg-primary-subtle font-semibold",
  emailRowJunk: "bg-warning-bg",
  junkRowBadge:
    "inline-flex shrink-0 items-center rounded-md bg-warning px-1.5 py-0.5 text-[0.65rem] font-bold tracking-wider text-white",
  emailSender: "overflow-hidden text-ellipsis whitespace-nowrap",
  emailSubject: "overflow-hidden text-ellipsis whitespace-nowrap text-fg",
  emailDate: "text-right text-xs text-fg-muted",
  dropTargetActive: "bg-primary-subtle outline-dashed outline-2 outline-primary",
  labelSection: "mt-4 border-t border-border pt-3",
  labelSectionHeader: "mb-2 flex items-center justify-between",
  labelSectionTitle:
    "text-xs font-bold uppercase tracking-wider text-fg-muted",
  labelManageLink: "text-xs text-primary no-underline hover:underline",
  labelClear:
    "w-full cursor-pointer rounded-md border-0 bg-transparent px-2 py-1 text-left text-xs text-fg-muted transition-colors hover:bg-surface-hover",
  labelNameWrap: "inline-flex items-center gap-1.5",
  labelDot: "size-3 shrink-0 rounded-pill",
  listControls: "mb-1 flex flex-wrap items-center gap-4 py-1",
  controlToggle:
    "inline-flex cursor-pointer items-center gap-1.5 text-xs text-fg-muted",
  filterPill:
    "inline-flex items-center gap-1.5 rounded-pill bg-primary-subtle px-2 py-0.5 text-xs text-on-accent",
  filterPillClear:
    "cursor-pointer border-0 bg-transparent text-sm leading-none text-on-accent",
  bulkBar:
    "mb-2 flex flex-wrap items-center gap-2 rounded-md border border-border bg-surface-muted px-2.5 py-2",
  bulkCount: "text-xs font-semibold text-fg-muted",
  bulkButton:
    "cursor-pointer rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs text-fg transition-colors hover:bg-surface-hover disabled:cursor-not-allowed disabled:opacity-60",
  bulkSelect:
    "rounded-md border border-border bg-surface px-2 py-1.5 text-xs text-fg",
  emailRowSelected: "bg-surface-active",
  rowCheckbox: "shrink-0 cursor-pointer",
  threadBadge:
    "ml-1.5 inline-flex h-[1.1rem] min-w-[1.1rem] items-center justify-center rounded-pill bg-fg-subtle px-1 text-[0.7rem] font-bold text-white",
  emailSubjectWrap: "flex items-center gap-1.5 overflow-hidden",
  rowLabelChip:
    "shrink-0 whitespace-nowrap rounded-pill px-1.5 py-0.5 text-[0.65rem] font-semibold text-white",
};
