/** Shared site metadata and navigation used across layouts. */

export const SITE = {
  name: "KMail",
  /** Short product positioning used in titles and the hero. */
  tagline: "Private business email for teams",
  description:
    "KMail is private, encrypted business email and calendar for teams — a privacy-first alternative to Gmail and Microsoft 365, with shared inboxes, zero-access vaults, and customer-managed keys.",
  /** Where the self-service signup funnel lives (served by the React app). */
  signupUrl: "/signup",
  /** Where the authenticated web app lives. */
  appUrl: "/mail",
  supportEmail: "support@kmail.kchat.dev",
};

export interface NavItem {
  label: string;
  href: string;
}

export const PRIMARY_NAV: NavItem[] = [
  { label: "Features", href: "/features" },
  { label: "Pricing", href: "/pricing" },
  { label: "Security", href: "/security" },
  { label: "Help", href: "/help" },
  { label: "API", href: "/docs/api" },
  { label: "Status", href: "/status" },
];

export const FOOTER_NAV: { heading: string; items: NavItem[] }[] = [
  {
    heading: "Product",
    items: [
      { label: "Features", href: "/features" },
      { label: "Pricing", href: "/pricing" },
      { label: "Security", href: "/security" },
      { label: "Privacy", href: "/privacy" },
      { label: "Changelog", href: "/changelog" },
    ],
  },
  {
    heading: "Developers",
    items: [
      { label: "API reference", href: "/docs/api" },
      { label: "Webhooks", href: "/help/admin/webhook-events" },
      { label: "SCIM provisioning", href: "/help/admin/scim-provisioning" },
      { label: "Status", href: "/status" },
    ],
  },
  {
    heading: "Help",
    items: [
      { label: "Help center", href: "/help" },
      { label: "DNS setup", href: "/help/getting-started/dns-setup" },
      { label: "Gmail migration", href: "/help/migration/gmail-migration" },
      { label: "M365 migration", href: "/help/migration/m365-migration" },
    ],
  },
  {
    heading: "Company",
    items: [
      { label: "About", href: "/about" },
      { label: "Blog", href: "/blog" },
      { label: "Status feed", href: "/status/feed.xml" },
    ],
  },
];
