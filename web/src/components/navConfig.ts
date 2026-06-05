/**
 * Navigation model for the KMail shell.
 *
 * A single source of truth describing the sidebar tree (used by
 * `Layout`) and the breadcrumb trail (derived via
 * {@link breadcrumbsForPath}). Keeping the structure declarative
 * means the 30+ flat links from the Phase-1 shell are now grouped
 * into collapsible sections without hand-maintaining markup.
 */

export interface NavLink {
  type: "link";
  label: string;
  to: string;
  /** Short text glyph used as the nav icon (kept dependency-free). */
  icon?: string;
}

export interface NavGroup {
  type: "group";
  id: string;
  label: string;
  icon?: string;
  children: NavNode[];
}

export type NavNode = NavLink | NavGroup;

export const NAV_TREE: NavNode[] = [
  {
    type: "group",
    id: "mail",
    label: "Mail",
    icon: "✉",
    children: [
      { type: "link", label: "Inbox", to: "/mail", icon: "📥" },
      { type: "link", label: "Compose", to: "/mail/compose", icon: "✏" },
      { type: "link", label: "Shared inboxes", to: "/mail/shared", icon: "👥" },
      { type: "link", label: "Zero-Access Vault", to: "/mail/vault", icon: "🔒" },
      {
        type: "link",
        label: "Protected folders",
        to: "/mail/protected-folders",
        icon: "🗂",
      },
      { type: "link", label: "Scheduled", to: "/mail/scheduled", icon: "⏰" },
      { type: "link", label: "Snoozed", to: "/mail/snoozed", icon: "😴" },
    ],
  },
  {
    type: "group",
    id: "calendar",
    label: "Calendar",
    icon: "📅",
    children: [
      { type: "link", label: "Calendar", to: "/calendar", icon: "📅" },
      { type: "link", label: "New event", to: "/calendar/new", icon: "➕" },
      {
        type: "link",
        label: "Shared calendars",
        to: "/calendar/shared",
        icon: "🤝",
      },
    ],
  },
  {
    type: "group",
    id: "contacts",
    label: "Contacts",
    icon: "👤",
    children: [{ type: "link", label: "Contacts", to: "/contacts", icon: "👤" }],
  },
  {
    type: "group",
    id: "admin",
    label: "Admin",
    icon: "⚙",
    children: [
      {
        type: "group",
        id: "admin-domains",
        label: "Domains",
        children: [
          { type: "link", label: "Domain admin", to: "/admin/domains" },
          { type: "link", label: "DNS wizard", to: "/admin/dns-wizard" },
          { type: "link", label: "DKIM keys", to: "/admin/dkim" },
          { type: "link", label: "DMARC", to: "/admin/dmarc" },
          { type: "link", label: "IP reputation", to: "/admin/ip-reputation" },
        ],
      },
      {
        type: "group",
        id: "admin-security",
        label: "Security",
        children: [
          { type: "link", label: "Security keys", to: "/admin/security" },
          { type: "link", label: "Customer-managed keys", to: "/admin/cmk" },
          { type: "link", label: "SCIM provisioning", to: "/admin/scim" },
          { type: "link", label: "Approvals", to: "/admin/approvals" },
        ],
      },
      {
        type: "group",
        id: "admin-billing",
        label: "Billing",
        children: [
          { type: "link", label: "Billing", to: "/admin/billing" },
          { type: "link", label: "Pricing", to: "/admin/pricing" },
          { type: "link", label: "Pricing & Plans", to: "/admin/pricing-plans" },
        ],
      },
      {
        type: "group",
        id: "admin-compliance",
        label: "Compliance",
        children: [
          { type: "link", label: "Audit log", to: "/admin/audit" },
          { type: "link", label: "Retention", to: "/admin/retention" },
          { type: "link", label: "Exports", to: "/admin/exports" },
          { type: "link", label: "Sieve rules", to: "/admin/sieve" },
        ],
      },
      {
        type: "group",
        id: "admin-operations",
        label: "Operations",
        children: [
          { type: "link", label: "Tenant admin", to: "/admin/tenant" },
          { type: "link", label: "User admin", to: "/admin/users" },
          { type: "link", label: "Migrations", to: "/admin/migrations" },
          {
            type: "link",
            label: "Resource calendars",
            to: "/admin/resource-calendars",
          },
          { type: "link", label: "SLO Dashboard", to: "/admin/slo" },
          {
            type: "link",
            label: "Storage placement",
            to: "/admin/storage-placement",
          },
          { type: "link", label: "Notifications", to: "/admin/notifications" },
          { type: "link", label: "Webhooks", to: "/admin/webhooks" },
          { type: "link", label: "Onboarding", to: "/admin/onboarding" },
          { type: "link", label: "Search backend", to: "/admin/search" },
        ],
      },
    ],
  },
];

export interface PathMeta {
  /** Trail of ancestor labels (e.g. ["Admin", "Domains"]). */
  trail: string[];
  /** Leaf label for the route itself. */
  label: string;
  to: string;
}

/** Flatten the tree into a `path -> meta` lookup for breadcrumbs. */
function buildPathIndex(): Map<string, PathMeta> {
  const index = new Map<string, PathMeta>();
  const walk = (nodes: NavNode[], trail: string[]): void => {
    for (const node of nodes) {
      if (node.type === "link") {
        index.set(node.to, { trail, label: node.label, to: node.to });
      } else {
        walk(node.children, [...trail, node.label]);
      }
    }
  };
  walk(NAV_TREE, []);
  return index;
}

const PATH_INDEX = buildPathIndex();

export interface BreadcrumbItem {
  label: string;
  to?: string;
}

/**
 * Build the breadcrumb trail for a pathname. Falls back gracefully
 * for dynamic routes (e.g. `/mail/:mailboxId/:emailId`) by matching
 * the longest known path prefix.
 */
export function breadcrumbsForPath(pathname: string): BreadcrumbItem[] {
  // Exact match first.
  let meta = PATH_INDEX.get(pathname);

  // Otherwise, find the closest known ancestor (longest prefix).
  if (!meta) {
    let best: PathMeta | undefined;
    for (const candidate of PATH_INDEX.values()) {
      if (
        pathname.startsWith(candidate.to + "/") &&
        (!best || candidate.to.length > best.to.length)
      ) {
        best = candidate;
      }
    }
    meta = best;
  }

  if (!meta) return [];
  const trail: BreadcrumbItem[] = meta.trail.map((label) => ({ label }));
  trail.push({ label: meta.label, to: meta.to });
  return trail;
}

/**
 * Group ids whose subtree contains a link matching `pathname`
 * (exact or as a path prefix). Used to auto-expand the sidebar
 * sections that lead to the active route.
 */
export function expandedGroupIdsForPath(pathname: string): string[] {
  const ids: string[] = [];
  const matches = (to: string): boolean =>
    pathname === to || pathname.startsWith(to + "/");

  const walk = (nodes: NavNode[]): boolean => {
    let anyMatch = false;
    for (const node of nodes) {
      if (node.type === "link") {
        if (matches(node.to)) anyMatch = true;
      } else if (walk(node.children)) {
        ids.push(node.id);
        anyMatch = true;
      }
    }
    return anyMatch;
  };
  walk(NAV_TREE);
  return ids;
}
