import { Suspense, useEffect, useMemo, useRef, useState } from "react";
import {
  NavLink,
  Outlet,
  useLocation,
  useNavigate,
} from "react-router-dom";
import {
  Building2,
  ChevronRight,
  Keyboard,
  KeyRound,
  Menu,
  Moon,
  Search,
  Sun,
} from "lucide-react";

import { Avatar } from "./ui/Avatar";
import { Dropdown } from "./ui/Dropdown";
import type { DropdownItem } from "./ui/Dropdown";
import { Tooltip } from "./ui/Tooltip";
import { Breadcrumbs } from "./Breadcrumbs";
import { RouteFallback } from "./RouteFallback";
import { ShortcutHelpModal } from "./ShortcutHelpModal";
import {
  NAV_TREE,
  breadcrumbsForPath,
  expandedGroupIdsForPath,
} from "./navConfig";
import type { NavNode } from "./navConfig";
import { cn } from "../lib/cn";
import { useTheme } from "../hooks/useTheme";
import { useIsMobile } from "../hooks/useMediaQuery";
import { useKeyboardShortcuts } from "../hooks/useKeyboardShortcuts";
import type { KeyboardShortcut } from "../hooks/useKeyboardShortcuts";

/**
 * Layout — the production shell around every authenticated KMail
 * page.
 *
 * Responsibilities:
 *   - Top bar: brand, global search, theme toggle, help, and the
 *     account dropdown.
 *   - Collapsible, grouped sidebar navigation (replaces the old
 *     flat 30-link `<ul>`), which becomes an off-canvas drawer on
 *     mobile.
 *   - ARIA landmarks (banner / navigation / main), a skip link,
 *     active-route highlighting, breadcrumbs, dark-mode toggle, and
 *     global keyboard shortcuts with a `?` help modal.
 *
 * Page bodies remain owned by their respective workstreams; this
 * component only provides the chrome and shared affordances.
 */
export default function Layout(): JSX.Element {
  const location = useLocation();
  const navigate = useNavigate();
  const isMobile = useIsMobile();
  const { resolvedTheme, toggleTheme } = useTheme();

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [openGroups, setOpenGroups] = useState<Set<string>>(() => {
    // Top-level groups start open; auto-expand the active subtree.
    const initial = new Set<string>(
      NAV_TREE.filter((n): n is Extract<NavNode, { type: "group" }> =>
        n.type === "group",
      ).map((n) => n.id),
    );
    for (const id of expandedGroupIdsForPath(location.pathname)) {
      initial.add(id);
    }
    return initial;
  });

  const searchRef = useRef<HTMLInputElement>(null);

  const breadcrumbs = breadcrumbsForPath(location.pathname);

  // Close the mobile drawer whenever the route changes.
  useEffect(() => {
    setDrawerOpen(false);
  }, [location.pathname]);

  // Ensure the active route's groups are expanded after navigation.
  useEffect(() => {
    setOpenGroups((prev) => {
      const next = new Set(prev);
      for (const id of expandedGroupIdsForPath(location.pathname)) {
        next.add(id);
      }
      return next;
    });
  }, [location.pathname]);

  const toggleGroup = (id: string): void => {
    setOpenGroups((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // ---- Keyboard shortcuts -------------------------------------------------
  const globalShortcuts: KeyboardShortcut[] = useMemo(
    () => [
      {
        keys: "c",
        description: "Compose new message",
        group: "Global",
        handler: () => navigate("/mail/compose"),
      },
      {
        keys: "g i",
        description: "Go to Inbox",
        group: "Navigation",
        handler: () => navigate("/mail"),
      },
      {
        keys: "g c",
        description: "Go to Calendar",
        group: "Navigation",
        handler: () => navigate("/calendar"),
      },
      {
        keys: "/",
        description: "Focus search",
        group: "Global",
        handler: () => searchRef.current?.focus(),
      },
      {
        keys: "?",
        description: "Show this help",
        group: "Global",
        handler: () => setHelpOpen(true),
      },
    ],
    [navigate],
  );

  // Display-only reference for page-level shortcuts owned by Mail
  // pages. Listed in the help modal so users discover them; the
  // actual handlers live in the owning pages.
  const referenceShortcuts: KeyboardShortcut[] = useMemo(
    () => [
      { keys: "r", description: "Reply", group: "Message", handler: () => {} },
      {
        keys: "a",
        description: "Reply all",
        group: "Message",
        handler: () => {},
      },
      { keys: "f", description: "Forward", group: "Message", handler: () => {} },
      { keys: "e", description: "Archive", group: "Message", handler: () => {} },
      { keys: "#", description: "Delete", group: "Message", handler: () => {} },
      {
        keys: "j",
        description: "Next message",
        group: "List",
        handler: () => {},
      },
      {
        keys: "k",
        description: "Previous message",
        group: "List",
        handler: () => {},
      },
    ],
    [],
  );

  useKeyboardShortcuts(globalShortcuts, { enabled: !helpOpen });

  const onSearchSubmit = (e: React.FormEvent): void => {
    e.preventDefault();
    const q = search.trim();
    navigate(q ? `/mail?q=${encodeURIComponent(q)}` : "/mail");
  };

  const accountItems: DropdownItem[] = [
    {
      id: "tenant",
      label: "Tenant admin",
      icon: <Building2 />,
      onSelect: () => navigate("/admin/tenant"),
    },
    {
      id: "security",
      label: "Security keys",
      icon: <KeyRound />,
      onSelect: () => navigate("/admin/security"),
    },
    {
      id: "theme",
      label: resolvedTheme === "dark" ? "Switch to light" : "Switch to dark",
      icon: resolvedTheme === "dark" ? <Sun /> : <Moon />,
      onSelect: toggleTheme,
      separatorBefore: true,
    },
    {
      id: "shortcuts",
      label: "Keyboard shortcuts",
      icon: <Keyboard />,
      onSelect: () => setHelpOpen(true),
    },
  ];

  const iconButton =
    "inline-flex size-11 items-center justify-center rounded-md bg-transparent text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring";

  return (
    <div className="flex min-h-screen flex-col bg-canvas text-fg">
      <a href="#kmail-main" className="skip-link">
        Skip to content
      </a>

      <header className="sticky top-0 z-header flex h-[var(--header-height)] items-center gap-4 border-b border-border bg-elevated px-4">
        <div className="flex flex-none items-center gap-2">
          {isMobile && (
            <button
              type="button"
              className={iconButton}
              aria-label={drawerOpen ? "Close menu" : "Open menu"}
              aria-expanded={drawerOpen}
              aria-controls="kmail-sidebar"
              onClick={() => setDrawerOpen((v) => !v)}
            >
              <Menu className="size-5" aria-hidden="true" />
            </button>
          )}
          <NavLink
            to="/mail"
            className="inline-flex items-center gap-2.5 text-lg font-bold text-fg hover:no-underline"
            aria-label="KMail home"
          >
            <span
              className="inline-flex size-8 items-center justify-center rounded-xl bg-primary text-primary-fg shadow-sm"
              aria-hidden="true"
            >
              <svg
                viewBox="0 0 24 24"
                className="size-5"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
              >
                <rect x="3" y="5" width="18" height="14" rx="3" />
                <path d="m21 7-7.97 5.7a1.94 1.94 0 0 1-2.06 0L3 7" />
              </svg>
            </span>
            <span className="max-sm:hidden">KMail</span>
          </NavLink>
        </div>

        <form
          className="relative flex max-w-xl flex-1 items-center"
          role="search"
          onSubmit={onSearchSubmit}
        >
          <Search
            className="pointer-events-none absolute left-3 size-4 text-fg-subtle"
            aria-hidden="true"
          />
          <input
            ref={searchRef}
            type="search"
            className="h-10 w-full rounded-pill border border-border bg-surface-muted pl-9 pr-3 text-sm text-fg outline-none transition-colors placeholder:text-fg-subtle focus-visible:border-primary focus-visible:bg-surface focus-visible:ring-2 focus-visible:ring-primary-subtle"
            placeholder="Search mail…  ( / )"
            aria-label="Search mail"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </form>

        <div className="flex flex-none items-center gap-1">
          <Tooltip
            label={resolvedTheme === "dark" ? "Light mode" : "Dark mode"}
          >
            <button
              type="button"
              className={iconButton}
              aria-label={
                resolvedTheme === "dark"
                  ? "Switch to light mode"
                  : "Switch to dark mode"
              }
              aria-pressed={resolvedTheme === "dark"}
              onClick={toggleTheme}
            >
              {resolvedTheme === "dark" ? (
                <Sun className="size-5" aria-hidden="true" />
              ) : (
                <Moon className="size-5" aria-hidden="true" />
              )}
            </button>
          </Tooltip>
          <Tooltip label="Keyboard shortcuts ( ? )">
            <button
              type="button"
              className={iconButton}
              aria-label="Keyboard shortcuts"
              onClick={() => setHelpOpen(true)}
            >
              <Keyboard className="size-5" aria-hidden="true" />
            </button>
          </Tooltip>
          <Dropdown
            ariaLabel="Account menu"
            align="end"
            trigger={
              <button
                type="button"
                className="inline-flex items-center rounded-pill bg-transparent p-1 transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                aria-label="Account menu"
              >
                <Avatar name="KMail User" size="sm" />
              </button>
            }
            items={accountItems}
          />
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        {/* Backdrop behind the mobile drawer. */}
        {isMobile && drawerOpen && (
          <div
            className="fixed inset-x-0 bottom-0 top-[var(--header-height)] z-drawer bg-overlay"
            onClick={() => setDrawerOpen(false)}
            aria-hidden="true"
          />
        )}

        <nav
          id="kmail-sidebar"
          className={cn(
            "w-[var(--sidebar-width)] flex-none overflow-y-auto border-r border-border bg-surface-muted py-3",
            isMobile &&
              "fixed bottom-0 left-0 top-[var(--header-height)] z-[301] -translate-x-full shadow-lg transition-transform duration-200",
            isMobile && drawerOpen && "translate-x-0",
          )}
          aria-label="Primary"
        >
          <ul className="flex flex-col gap-px">
            {NAV_TREE.map((node) => (
              <NavTreeNode
                key={node.type === "group" ? node.id : node.to}
                node={node}
                depth={0}
                openGroups={openGroups}
                onToggle={toggleGroup}
              />
            ))}
          </ul>
        </nav>

        <main
          id="kmail-main"
          className="flex min-w-0 flex-1 flex-col focus:outline-none"
          tabIndex={-1}
        >
          {breadcrumbs.length > 0 && (
            <div className="border-b border-border bg-canvas px-6 py-3 max-sm:px-4 max-sm:py-2">
              <Breadcrumbs items={breadcrumbs} />
            </div>
          )}
          <div className="flex-1 p-6 max-sm:p-4">
            {/* Route pages are code-split (see App.tsx); this boundary
                keeps the nav + chrome mounted and shows a skeleton in
                the content area while the next page's chunk loads. */}
            <Suspense fallback={<RouteFallback />}>
              <Outlet />
            </Suspense>
          </div>
        </main>
      </div>

      <ShortcutHelpModal
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        shortcuts={[...globalShortcuts, ...referenceShortcuts]}
      />
    </div>
  );
}

interface NavTreeNodeProps {
  node: NavNode;
  depth: number;
  openGroups: Set<string>;
  onToggle: (id: string) => void;
}

/** Recursive sidebar renderer: links + collapsible groups. */
function NavTreeNode({
  node,
  depth,
  openGroups,
  onToggle,
}: NavTreeNodeProps): JSX.Element {
  if (node.type === "link") {
    const Icon = node.icon;
    return (
      <li>
        <NavLink
          to={node.to}
          end
          className={({ isActive }) =>
            cn(
              "flex min-h-10 items-center gap-2 border-l-[3px] border-transparent py-2 pr-4 text-sm text-fg-muted transition-colors hover:bg-surface-hover hover:text-fg hover:no-underline",
              isActive &&
                "border-primary bg-surface-active font-semibold text-primary",
            )
          }
          style={{ paddingLeft: `calc(var(--space-4) + ${depth} * 0.75rem)` }}
        >
          {Icon && (
            <span
              className="inline-flex w-5 flex-none justify-center"
              aria-hidden="true"
            >
              <Icon className="size-[1.05rem]" />
            </span>
          )}
          <span className="flex-1 truncate">{node.label}</span>
        </NavLink>
      </li>
    );
  }

  const expanded = openGroups.has(node.id);
  const sectionId = `nav-group-${node.id}`;
  const Icon = node.icon;
  return (
    <li className="flex flex-col">
      <button
        type="button"
        className={cn(
          "flex min-h-10 w-full items-center gap-2 py-2 pr-4 text-left text-sm font-semibold text-fg transition-colors hover:bg-surface-hover",
          depth > 0 &&
            "text-xs font-semibold uppercase tracking-wide text-fg-muted",
        )}
        aria-expanded={expanded}
        // Only reference the sub-list while it's actually in the DOM
        // (it's unmounted when collapsed), so aria-controls never
        // points at a missing element.
        aria-controls={expanded ? sectionId : undefined}
        onClick={() => onToggle(node.id)}
        style={{ paddingLeft: `calc(var(--space-4) + ${depth} * 0.75rem)` }}
      >
        {Icon && (
          <span
            className="inline-flex w-5 flex-none justify-center"
            aria-hidden="true"
          >
            <Icon className="size-[1.05rem]" />
          </span>
        )}
        <span className="flex-1 truncate">{node.label}</span>
        <ChevronRight
          className={cn(
            "ml-auto size-4 text-fg-subtle transition-transform",
            expanded && "rotate-90",
          )}
          aria-hidden="true"
        />
      </button>
      {expanded && (
        <ul id={sectionId} className="flex flex-col gap-px">
          {node.children.map((child) => (
            <NavTreeNode
              key={child.type === "group" ? child.id : child.to}
              node={child}
              depth={depth + 1}
              openGroups={openGroups}
              onToggle={onToggle}
            />
          ))}
        </ul>
      )}
    </li>
  );
}
