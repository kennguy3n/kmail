/**
 * Shared TypeScript types for the KMail React client.
 *
 * These types describe the shape of the Go BFF's JMAP surface
 * (see docs/JMAP-CONTRACT.md). Where the BFF contract is a
 * faithful pass-through of RFC 8621 we mirror the spec names and
 * field layout exactly; where the BFF adds tenant-scoped fields
 * (e.g. `privacyMode`) we extend the spec type in-place and note
 * the divergence in the doc-comment above the field.
 */

/** A JMAP method invocation in a batch (RFC 8620 §3.2). */
export type JmapInvocation = [
  method: string,
  args: Record<string, unknown>,
  callId: string,
];

/** The response shape for a single method invocation. */
export type JmapResponseInvocation = [
  method: string,
  result: Record<string, unknown>,
  callId: string,
];

/** Top-level `/jmap/api` response envelope (RFC 8620 §3.3). */
export interface JmapResponse {
  methodResponses: JmapResponseInvocation[];
  sessionState: string;
  createdIds?: Record<string, string>;
}

/** The JMAP session object the BFF returns at the session URL. */
export interface JmapSession {
  capabilities: Record<string, unknown>;
  accounts: Record<string, JmapAccount>;
  primaryAccounts: Record<string, string>;
  username: string;
  apiUrl: string;
  downloadUrl: string;
  uploadUrl: string;
  eventSourceUrl: string;
  state: string;
}

/** A JMAP account advertised in the session object. */
export interface JmapAccount {
  name: string;
  isPersonal: boolean;
  isReadOnly: boolean;
  accountCapabilities: Record<string, unknown>;
}

/** Well-known JMAP Mail capability URI (RFC 8621 §2). */
export const JMAP_MAIL_CAPABILITY = "urn:ietf:params:jmap:mail";
/** Well-known JMAP Submission capability URI (RFC 8621 §7). */
export const JMAP_SUBMISSION_CAPABILITY =
  "urn:ietf:params:jmap:submission";
/**
 * JMAP Calendars capability URI
 * (https://datatracker.ietf.org/doc/draft-ietf-jmap-calendars/).
 * KMail advertises this capability through its Go BFF per
 * docs/JMAP-CONTRACT.md §2.1; the underlying CalDAV store is
 * Stalwart's (mail-server v0.16.0 ships a CalDAV implementation
 * but does not yet advertise a `urn:ietf:params:jmap:calendars`
 * capability of its own, so the BFF is expected to expose the
 * capability on top of the CalDAV store until upstream parity
 * lands). The React client uses this URI to scope `Calendar/*`
 * and `CalendarEvent/*` method calls and to discover the
 * calendar account ID from the session object.
 */
export const JMAP_CALENDARS_CAPABILITY =
  "urn:ietf:params:jmap:calendars";

/** KMail tenant plan, mirrored from `tenants.plan` in docs/SCHEMA.md. */
export type TenantPlan = "core" | "pro" | "privacy";

/** Privacy mode for a mailbox or message. */
export type PrivacyMode =
  | "standard"
  | "confidential-send"
  | "zero-access-vault";

/**
 * A JMAP Mailbox (RFC 8621 §2). Fields match the spec; `role` is a
 * free-form string because the spec allows user-defined roles in
 * addition to the well-known set (inbox, archive, drafts, sent,
 * trash, junk, important, flagged).
 */
export interface Mailbox {
  id: string;
  name: string;
  parentId: string | null;
  role: string | null;
  sortOrder: number;
  totalEmails: number;
  unreadEmails: number;
  totalThreads: number;
  unreadThreads: number;
  myRights: MailboxRights;
  isSubscribed: boolean;
}

/** RFC 8621 §2 MailboxRights object. */
export interface MailboxRights {
  mayReadItems: boolean;
  mayAddItems: boolean;
  mayRemoveItems: boolean;
  maySetSeen: boolean;
  maySetKeywords: boolean;
  mayCreateChild: boolean;
  mayRename: boolean;
  mayDelete: boolean;
  maySubmit: boolean;
}

/** RFC 8621 §4.1.2 EmailAddress. */
export interface EmailAddress {
  name: string | null;
  email: string;
}

/**
 * RFC 8621 §4.1.4 EmailBodyPart. A message's body is modelled as a
 * tree of these parts; leaves carry the `partId` or `blobId` that
 * lets the client fetch the content.
 */
export interface EmailBodyPart {
  partId: string | null;
  blobId: string | null;
  size: number;
  headers?: EmailHeader[];
  name: string | null;
  type: string;
  charset: string | null;
  disposition: string | null;
  cid: string | null;
  language: string[] | null;
  location: string | null;
  subParts: EmailBodyPart[] | null;
}

/** RFC 8621 §4.1.3 EmailHeader. */
export interface EmailHeader {
  name: string;
  value: string;
}

/**
 * RFC 8621 §4.1.1 Email object. Narrowed to the fields the KMail
 * inbox/message views need today; unknown spec fields pass through
 * on the wire but are not surfaced here until a UI needs them.
 *
 * `privacyMode` is a KMail extension (not in RFC 8621) carrying the
 * privacy-mode tag the BFF resolves from the mailbox / message
 * headers. The field is optional so generic JMAP callers keep
 * working.
 */
export interface Email {
  id: string;
  blobId: string;
  threadId: string;
  mailboxIds: Record<string, boolean>;
  keywords: Record<string, boolean>;
  size: number;
  receivedAt: string;
  from: EmailAddress[] | null;
  to: EmailAddress[] | null;
  cc: EmailAddress[] | null;
  bcc: EmailAddress[] | null;
  replyTo: EmailAddress[] | null;
  subject: string | null;
  sentAt: string | null;
  bodyStructure?: EmailBodyPart;
  bodyValues?: Record<string, EmailBodyValue>;
  textBody?: EmailBodyPart[];
  htmlBody?: EmailBodyPart[];
  attachments?: EmailBodyPart[];
  hasAttachment?: boolean;
  preview?: string;
  privacyMode?: PrivacyMode;
}

/** RFC 8621 §4.1.4 EmailBodyValue. */
export interface EmailBodyValue {
  value: string;
  isEncodingProblem: boolean;
  isTruncated: boolean;
}

/** Shape accepted by JMAPClient.sendEmail() for a new draft. */
export interface EmailDraft {
  mailboxIds: Record<string, boolean>;
  from?: EmailAddress[];
  to: EmailAddress[];
  cc?: EmailAddress[];
  bcc?: EmailAddress[];
  subject: string;
  textBody?: string;
  htmlBody?: string;
  privacyMode?: PrivacyMode;
  /**
   * Explicit Identity id to send under. When omitted, the client
   * resolves the account's default identity via `Identity/get`
   * (see RFC 8621 §6) and uses that — callers that need a
   * non-default identity must set this field.
   */
  identityId?: string;
}

/**
 * RFC 8621 §6.1 Identity object. Narrowed to the fields the client
 * actually consults; unknown fields pass through the wire.
 */
export interface Identity {
  id: string;
  name: string;
  email: string;
  replyTo: EmailAddress[] | null;
  bcc: EmailAddress[] | null;
  textSignature: string | null;
  htmlSignature: string | null;
  mayDelete: boolean;
}

/** Options accepted by JMAPClient.getEmails() for list-view queries. */
export interface GetEmailsOptions {
  /** Max results per page; default 50. */
  limit?: number;
  /** Offset into the Email/query result set; default 0. */
  position?: number;
  /** Sort order; default [{ property: "receivedAt", isAscending: false }]. */
  sort?: EmailSort[];
}

/** RFC 8620 §5 sort comparator, narrowed to the Email properties we use. */
export interface EmailSort {
  property: "receivedAt" | "sentAt" | "size" | "subject";
  isAscending?: boolean;
}

/**
 * Options accepted by `JMAPClient.searchEmails()`.
 *
 * `text` is the user-visible full-text query; the BFF passes it
 * through to Stalwart as an RFC 8621 §4.4.1 `FilterCondition.text`
 * term. When `mailboxId` is supplied the search is scoped to that
 * mailbox (`inMailbox` AND `text`); when omitted the search spans
 * every visible mailbox for the account (global search). Vault
 * mailboxes are rejected server-side — see
 * docs/JMAP-CONTRACT.md §2.4.
 */
export interface SearchEmailsOptions {
  /** Max results per page; default 50. */
  limit?: number;
  /** Offset into the Email/query result set; default 0. */
  position?: number;
  /**
   * Scope the search to a single mailbox id. Omit for a global
   * search across every mailbox the authenticated user can see.
   */
  mailboxId?: string | null;
  /** Sort order; default [{ property: "receivedAt", isAscending: false }]. */
  sort?: EmailSort[];
}

/**
 * A calendar belonging to the authenticated user.
 *
 * Mirrors the draft JMAP calendars spec: every calendar has a
 * server-assigned `id`, a human-readable `name`, a CSS-compatible
 * `color`, an `isVisible` flag the UI uses to gate whether to
 * request events from that calendar, and an `isDefault` flag the
 * BFF sets on exactly one calendar per account.
 *
 * Stalwart v0.16.0 ships a CalDAV store but does not yet advertise
 * a JMAP calendars capability — the Go BFF is expected to surface
 * these objects on top of CalDAV collections until upstream parity
 * lands. The React client works against the JMAP shapes today and
 * the BFF swaps its backend without a UI change.
 */
export interface Calendar {
  id: string;
  name: string;
  color: string;
  isVisible: boolean;
  isDefault: boolean;
}

/**
 * A participant on a calendar event.
 *
 * Matches the draft JMAP calendars `Participant` object narrowed
 * to the fields the UI consults. `email` is the SMTP address the
 * invite is sent to; `name` is a human-readable label; `role`
 * tracks RFC 5545 PARTSTAT semantics (`required`, `optional`, or
 * `chair` for the organizer); `rsvp` carries the invitee's current
 * response.
 */
export interface EventParticipant {
  email: string;
  name?: string | null;
  role?: "chair" | "required" | "optional";
  rsvp?: EventParticipantResponse;
}

/**
 * Invitee response on a calendar event. `needs-action` is the
 * default state when the invite has been delivered but not yet
 * answered. Mirrors RFC 5545 `PARTSTAT` values.
 */
export type EventParticipantResponse =
  | "accepted"
  | "declined"
  | "tentative"
  | "needs-action";

/**
 * Draft `RecurrenceRule` sketch (see RFC 5545 §3.3.10 / draft
 * JMAP calendars). Narrowed to the properties the Phase 2 compose
 * form needs (`frequency`, `count`, `until`, `byDay`, `interval`).
 * Clients that don't recognise a field pass it through unchanged.
 */
export interface RecurrenceRule {
  frequency:
    | "yearly"
    | "monthly"
    | "weekly"
    | "daily"
    | "hourly"
    | "minutely"
    | "secondly";
  interval?: number;
  count?: number;
  until?: string;
  byDay?: string[];
}

/**
 * A calendar event. `start` / `end` are ISO-8601 timestamps in the
 * event's authoritative timezone; the UI renders them in the
 * viewer's local timezone. `status` tracks RFC 5545 STATUS
 * (`confirmed`, `tentative`, `cancelled`). `recurrenceRules` is
 * non-null for recurring events; the UI expands instances
 * client-side for Phase 2 and defers server-side expansion to
 * Phase 3.
 */
export interface CalendarEvent {
  id: string;
  calendarId: string;
  title: string;
  description?: string | null;
  start: string;
  end: string;
  location?: string | null;
  participants?: EventParticipant[];
  status?: "confirmed" | "tentative" | "cancelled";
  recurrenceRules?: RecurrenceRule[] | null;
}

/**
 * Shape accepted by `JMAPClient.createEvent()` /
 * `updateEvent()`. Omits the server-assigned `id` on create;
 * `calendarId` is required on create and optional on update
 * (to move an event between calendars).
 */
export interface CalendarEventDraft {
  calendarId: string;
  title: string;
  description?: string;
  start: string;
  end: string;
  location?: string;
  participants?: EventParticipant[];
  status?: "confirmed" | "tentative" | "cancelled";
  recurrenceRules?: RecurrenceRule[];
}

/**
 * Date range used by `JMAPClient.getEvents()`. Both bounds are
 * inclusive ISO-8601 timestamps. The BFF translates this to the
 * draft JMAP `CalendarEvent/query` filter
 * `{ after: start, before: end }`.
 */
export interface EventDateRange {
  start: string;
  end: string;
}

// ----------------------------------------------------------------
// Admin console (Phase 3)
// ----------------------------------------------------------------

/**
 * Tenant record as returned by the Go tenant service
 * (internal/tenant/service.go). The plan field drives per-seat
 * pricing: core ($3), pro ($6), privacy ($9).
 */
export interface Tenant {
  id: string;
  name: string;
  slug: string;
  plan: TenantPlan;
  status: "active" | "suspended" | "deleted";
  createdAt: string;
  updatedAt: string;
}

/** Fields the tenant admin form can patch. */
export interface TenantPatch {
  name?: string;
  plan?: TenantPlan;
  status?: "active" | "suspended" | "deleted";
}

/**
 * User record scoped to a tenant. `mailboxQuotaBytes` is the
 * soft quota the BFF enforces before Stalwart's own limits.
 */
export interface User {
  id: string;
  tenantId: string;
  email: string;
  displayName: string;
  role: "member" | "admin" | "owner";
  status: "active" | "suspended";
  mailboxQuotaBytes: number;
  createdAt: string;
  updatedAt: string;
}

/** Fields the user admin form can patch. */
export interface UserPatch {
  displayName?: string;
  role?: "member" | "admin" | "owner";
  status?: "active" | "suspended";
  mailboxQuotaBytes?: number;
}

/** Domain record as returned by the DNS onboarding service. */
export interface Domain {
  id: string;
  tenantId: string;
  domain: string;
  mxVerified: boolean;
  spfVerified: boolean;
  dkimVerified: boolean;
  dmarcVerified: boolean;
  verifiedAt?: string | null;
  createdAt: string;
  updatedAt: string;
}

/** One record in the DNS-record instructions table. */
export interface DnsRecord {
  type: "MX" | "TXT" | "CNAME";
  host: string;
  value: string;
  priority?: number | null;
  purpose: "mx" | "spf" | "dkim" | "dmarc";
}

/**
 * Verification result from a POST to
 * `/api/v1/tenants/{id}/domains/{domainId}/verify`.
 */
export interface VerifyDomainResult {
  mx: boolean;
  spf: boolean;
  dkim: boolean;
  dmarc: boolean;
  messages?: Record<string, string>;
}

/** Shared-inbox record (team mailboxes). */
export interface SharedInbox {
  id: string;
  tenantId: string;
  address: string;
  displayName: string;
  createdAt: string;
}

/** One row in the audit log. */
export interface AuditLogEntry {
  id: string;
  tenantId: string;
  actorId: string;
  actorType: "user" | "admin" | "system";
  action: string;
  resourceType: string;
  resourceId?: string;
  metadata?: Record<string, unknown>;
  ipAddress?: string;
  userAgent?: string;
  prevHash?: string;
  entryHash?: string;
  createdAt: string;
}

/** Optional filters for the audit-log query endpoint. */
export interface AuditLogQuery {
  action?: string;
  actor?: string;
  resource?: string;
  since?: string;
  until?: string;
  limit?: number;
  offset?: number;
}
