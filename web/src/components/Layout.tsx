import { useEffect, useMemo, useRef, useState } from "react";
import {
  NavLink,
  Outlet,
  useLocation,
  useNavigate,
} from "react-router-dom";

import { Avatar } from "./ui/Avatar";
import { Dropdown } from "./ui/Dropdown";
import type { DropdownItem } from "./ui/Dropdown";
import { Tooltip } from "./ui/Tooltip";
import { Breadcrumbs } from "./Breadcrumbs";
import { ShortcutHelpModal } from "./ShortcutHelpModal";
import {
  NAV_TREE,
  breadcrumbsForPath,
  expandedGroupIdsForPath,
} from "./navConfig";
import type { NavNode } from "./navConfig";
import { useTheme } from "../hooks/useTheme";
import { useIsMobile } from "../hooks/useMediaQuery";
import { useKeyboardShortcuts } from "../hooks/useKeyboardShortcuts";
import type { KeyboardShortcut } from "../hooks/useKeyboardShortcuts";
import styles from "./Layout.module.css";

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

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
      icon: "🏢",
      onSelect: () => navigate("/admin/tenant"),
    },
    {
      id: "security",
      label: "Security keys",
      icon: "🔑",
      onSelect: () => navigate("/admin/security"),
    },
    {
      id: "theme",
      label: resolvedTheme === "dark" ? "Switch to light" : "Switch to dark",
      icon: resolvedTheme === "dark" ? "☀" : "🌙",
      onSelect: toggleTheme,
      separatorBefore: true,
    },
    {
      id: "shortcuts",
      label: "Keyboard shortcuts",
      icon: "⌨",
      onSelect: () => setHelpOpen(true),
    },
  ];

  return (
    <div className={styles.shell}>
      <a href="#kmail-main" className="skip-link">
        Skip to content
      </a>

      <header className={styles.header}>
        <div className={styles.headerLeft}>
          {isMobile && (
            <button
              type="button"
              className={styles.iconButton}
              aria-label={drawerOpen ? "Close menu" : "Open menu"}
              aria-expanded={drawerOpen}
              aria-controls="kmail-sidebar"
              onClick={() => setDrawerOpen((v) => !v)}
            >
              ☰
            </button>
          )}
          <NavLink to="/mail" className={styles.brand}>
            <span className={styles.brandMark} aria-hidden="true">
              ✉
            </span>
            <span className={styles.brandName}>KMail</span>
          </NavLink>
        </div>

        <form
          className={styles.search}
          role="search"
          onSubmit={onSearchSubmit}
        >
          <span className={styles.searchIcon} aria-hidden="true">
            🔍
          </span>
          <input
            ref={searchRef}
            type="search"
            className={styles.searchInput}
            placeholder="Search mail…  ( / )"
            aria-label="Search mail"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </form>

        <div className={styles.headerRight}>
          <Tooltip
            label={resolvedTheme === "dark" ? "Light mode" : "Dark mode"}
          >
            <button
              type="button"
              className={styles.iconButton}
              aria-label={
                resolvedTheme === "dark"
                  ? "Switch to light mode"
                  : "Switch to dark mode"
              }
              aria-pressed={resolvedTheme === "dark"}
              onClick={toggleTheme}
            >
              {resolvedTheme === "dark" ? "☀" : "🌙"}
            </button>
          </Tooltip>
          <Tooltip label="Keyboard shortcuts ( ? )">
            <button
              type="button"
              className={styles.iconButton}
              aria-label="Keyboard shortcuts"
              onClick={() => setHelpOpen(true)}
            >
              ?
            </button>
          </Tooltip>
          <Dropdown
            ariaLabel="Account menu"
            align="end"
            trigger={
              <button
                type="button"
                className={styles.accountButton}
                aria-label="Account menu"
              >
                <Avatar name="KMail User" size="sm" />
              </button>
            }
            items={accountItems}
          />
        </div>
      </header>

      <div className={styles.body}>
        {/* Backdrop behind the mobile drawer. */}
        {isMobile && drawerOpen && (
          <div
            className={styles.backdrop}
            onClick={() => setDrawerOpen(false)}
            aria-hidden="true"
          />
        )}

        <nav
          id="kmail-sidebar"
          className={cx(
            styles.sidebar,
            isMobile && styles.sidebarMobile,
            isMobile && drawerOpen && styles.sidebarOpen,
          )}
          aria-label="Primary"
        >
          <ul className={styles.navList}>
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

        <main id="kmail-main" className={styles.main} tabIndex={-1}>
          {breadcrumbs.length > 0 && (
            <div className={styles.breadcrumbBar}>
              <Breadcrumbs items={breadcrumbs} />
            </div>
          )}
          <div className={styles.content}>
            <Outlet />
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
    return (
      <li>
        <NavLink
          to={node.to}
          end
          className={({ isActive }) =>
            cx(styles.navLink, isActive && styles.navLinkActive)
          }
          style={{ paddingLeft: `calc(var(--space-4) + ${depth} * 0.75rem)` }}
        >
          {node.icon && (
            <span className={styles.navIcon} aria-hidden="true">
              {node.icon}
            </span>
          )}
          <span className={styles.navLabel}>{node.label}</span>
        </NavLink>
      </li>
    );
  }

  const expanded = openGroups.has(node.id);
  const sectionId = `nav-group-${node.id}`;
  return (
    <li className={styles.group}>
      <button
        type="button"
        className={cx(styles.groupHeader, depth > 0 && styles.groupHeaderSub)}
        aria-expanded={expanded}
        aria-controls={sectionId}
        onClick={() => onToggle(node.id)}
        style={{ paddingLeft: `calc(var(--space-4) + ${depth} * 0.75rem)` }}
      >
        {node.icon && (
          <span className={styles.navIcon} aria-hidden="true">
            {node.icon}
          </span>
        )}
        <span className={styles.navLabel}>{node.label}</span>
        <span
          className={cx(styles.chevron, expanded && styles.chevronOpen)}
          aria-hidden="true"
        >
          ▸
        </span>
      </button>
      {expanded && (
        <ul id={sectionId} className={styles.subList}>
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
