/**
 * Plan catalog for the public pricing surface.
 *
 * This MUST stay in sync with the authoritative server-side catalog
 * (`web/src/api/admin.ts` → `PLAN_CATALOG`, validated against
 * `internal/tenant/signup.go`). Prices are expressed per seat / month.
 * The site is static, so we mirror the catalog here rather than
 * fetching it at runtime; the BFF remains the source of truth and
 * re-validates the chosen plan on `POST /api/v1/signup`.
 */

export type PlanId = "core" | "pro" | "privacy";

export interface Plan {
  id: PlanId;
  /** Marketing name shown on the pricing cards. */
  name: string;
  /** Whole-dollar monthly price per seat (USD). */
  priceUsd: number;
  tagline: string;
  storagePerSeatGB: number;
  dailySendLimit: number;
  /** Highlighted bullet features for the pricing card. */
  features: string[];
  /** Marks the recommended tier (visual emphasis only). */
  highlighted?: boolean;
}

export const PLANS: Plan[] = [
  {
    id: "core",
    name: "Core",
    priceUsd: 3,
    tagline: "Privacy-first email and calendar for small teams.",
    storagePerSeatGB: 5,
    dailySendLimit: 500,
    features: [
      "Encrypted mailboxes",
      "Custom domain + DNS wizard",
      "IMAP / SMTP / JMAP access",
      "Shared inboxes",
      "Basic spam filtering",
    ],
  },
  {
    id: "pro",
    name: "Pro",
    priceUsd: 6,
    tagline: "Advanced controls and higher quotas for growing businesses.",
    storagePerSeatGB: 15,
    dailySendLimit: 2000,
    highlighted: true,
    features: [
      "Everything in Core",
      "CalDAV / CardDAV calendars + contacts",
      "Advanced spam + DNSBL",
      "Migration automation (Gmail / M365)",
      "Custom retention policies",
      "Priority deliverability",
    ],
  },
  {
    id: "privacy",
    name: "Privacy",
    priceUsd: 9,
    tagline:
      "Zero-access vaults and client-side encryption for the privacy-obsessed.",
    storagePerSeatGB: 50,
    dailySendLimit: 5000,
    features: [
      "Everything in Pro",
      "Confidential Send (StrictZK)",
      "Zero-Access Vault",
      "Customer-managed keys (CMK / HSM)",
      "Dedicated IP pool",
      "DMARC reporting",
    ],
  },
];

/** Feature comparison rows for the detailed matrix on /pricing. */
export interface ComparisonRow {
  label: string;
  values: Record<PlanId, string>;
}

export const COMPARISON: ComparisonRow[] = [
  { label: "Price / seat / month", values: { core: "$3", pro: "$6", privacy: "$9" } },
  { label: "Storage / seat", values: { core: "5 GB", pro: "15 GB", privacy: "50 GB" } },
  { label: "Daily send limit", values: { core: "500", pro: "2,000", privacy: "5,000" } },
  { label: "Custom domain", values: { core: "Yes", pro: "Yes", privacy: "Yes" } },
  { label: "Shared inboxes", values: { core: "Yes", pro: "Yes", privacy: "Yes" } },
  { label: "Calendar / CalDAV", values: { core: "Yes", pro: "Yes", privacy: "Yes" } },
  { label: "Contacts / CardDAV", values: { core: "—", pro: "Yes", privacy: "Yes" } },
  { label: "Spam filtering", values: { core: "Basic", pro: "Advanced + DNSBL", privacy: "Advanced + DNSBL" } },
  { label: "Migration automation", values: { core: "—", pro: "Yes", privacy: "Yes" } },
  { label: "Custom retention policies", values: { core: "—", pro: "Yes", privacy: "Yes" } },
  { label: "Confidential Send (StrictZK)", values: { core: "—", pro: "—", privacy: "Yes" } },
  { label: "Zero-Access Vault", values: { core: "—", pro: "—", privacy: "Yes" } },
  { label: "Customer-managed keys (CMK)", values: { core: "—", pro: "—", privacy: "Yes" } },
  { label: "Dedicated IP pool", values: { core: "—", pro: "—", privacy: "Yes" } },
  { label: "DMARC reporting", values: { core: "—", pro: "—", privacy: "Yes" } },
  { label: "Priority support", values: { core: "—", pro: "Yes", privacy: "Yes" } },
];
