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
  /**
   * Read-receipt request header (RFC 8098): present when the sender
   * asked for a Message Disposition Notification. MessageView reads
   * it to decide whether to offer to send a receipt. Populated only
   * when explicitly requested via `header:*:asText` in `Email/get`.
   */
  "header:Disposition-Notification-To:asText"?: string | null;
  /** RFC 5322 Message-ID, used as the MDN's `Original-Message-ID`. */
  "header:Message-ID:asText"?: string | null;
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
  /**
   * When set, request a read receipt (RFC 8098 Message Disposition
   * Notification). The address is stamped into both the
   * `Disposition-Notification-To` and `Return-Receipt-To` headers
   * of the outgoing message via JMAP `header:*:asText` properties.
   */
  readReceiptTo?: string;
  /**
   * Attachments already uploaded to the JMAP blob store. Each entry
   * is emitted as an `attachment` body part referencing the blob by
   * id (RFC 8621 §4.1.4), so the recipient sees a real MIME
   * attachment rather than an inline link.
   */
  attachments?: DraftAttachment[];
}

/** An attachment referenced by a JMAP blob id for an outgoing draft. */
export interface DraftAttachment {
  blobId: string;
  name: string;
  type: string;
  size?: number;
  /** When true, the part is `inline` (Content-Disposition) for cid: images. */
  inline?: boolean;
  /** Content-ID used to reference an inline image from the HTML body. */
  cid?: string;
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

/**
 * CardDAV address book metadata returned by the contact bridge.
 */
export interface AddressBook {
  id: string;
  name: string;
  description?: string;
  isDefault: boolean;
}

/**
 * Slim view of a vCard surfaced by the BFF. The full vCard
 * payload is preserved in `vcardRaw` so unknown properties round
 * trip on update.
 */
export interface Contact {
  uid: string;
  fn: string;
  emails?: string[];
  phones?: string[];
  org?: string;
  note?: string;
  photoUrl?: string;
  groups?: string[];
  vcardRaw?: string;
}

/**
 * Input shape for create / update contact. UID is optional on
 * create — the BFF will assign one.
 */
export interface ContactDraft {
  uid?: string;
  fn: string;
  emails?: string[];
  phones?: string[];
  org?: string;
  note?: string;
  photoUrl?: string;
  groups?: string[];
}

/**
 * Tenant-wide directory entry surfaced by the Global Address List
 * (`internal/contactbridge/gal.go`). Read-only and deduplicated by
 * email within a tenant.
 */
export interface GalEntry {
  email: string;
  display_name?: string;
  org?: string;
  phone?: string;
  source_uid?: string;
  source_account?: string;
  last_synced_at?: string;
}

// ---------------------------------------------------------------
// WS2 — Email core features (feature depth)
// ---------------------------------------------------------------

/**
 * RFC 8621 §3 Thread object. A Thread groups the Emails that belong
 * to the same conversation; the BFF exposes `Thread/get` as a
 * faithful pass-through of the spec. `emailIds` is ordered by the
 * server in receivedAt-ascending order (RFC 8621 §3.2), which is
 * the order the conversation view renders messages in.
 */
export interface Thread {
  id: string;
  emailIds: string[];
}

/**
 * A reusable email signature. Stored client-side (localStorage) in
 * the first phase per the WS2 plan; the shape is intentionally a
 * faithful subset of what a future `internal/jmap/signature.go`
 * preferences row would persist, so the storage backend can be
 * swapped without touching callers.
 *
 * `identityEmail` scopes a signature to a specific From address
 * (RFC 8621 §6 Identity.email). A signature with `isDefault: true`
 * for an identity is auto-appended on compose/reply/forward when
 * the user sends under that identity; at most one default exists
 * per identity (enforced by the store on save).
 */
export interface Signature {
  id: string;
  name: string;
  /** Rich-text (HTML) signature body. */
  html: string;
  /** Identity (From) email this signature is scoped to; null = any. */
  identityEmail: string | null;
  isDefault: boolean;
  createdAt: string;
  updatedAt: string;
}

/** Fields a caller supplies when creating/updating a {@link Signature}. */
export interface SignatureDraft {
  name: string;
  html: string;
  identityEmail: string | null;
  isDefault: boolean;
}

/**
 * A reusable email template. `body` is HTML and may contain
 * `{{variable}}` placeholders that {@link renderTemplate} expands
 * against a value map (built-ins: `sender_name`, `company`,
 * `date`). `scope` distinguishes a user's private templates from
 * tenant-shared (admin-managed) ones.
 */
export interface EmailTemplate {
  id: string;
  name: string;
  subject: string;
  body: string;
  scope: "personal" | "shared";
  createdAt: string;
  updatedAt: string;
}

/** Fields a caller supplies when creating/updating an {@link EmailTemplate}. */
export interface EmailTemplateDraft {
  name: string;
  subject: string;
  body: string;
  scope: "personal" | "shared";
}

/**
 * A user-defined label/tag. Labels are applied to Emails as JMAP
 * keywords (RFC 8621 §4.1.1 `keywords`); the keyword token is
 * derived from the label name via {@link labelKeyword}. The
 * display name and `color` are kept in a small client-side store
 * because JMAP keywords carry no presentation metadata.
 */
export interface Label {
  id: string;
  name: string;
  /** CSS color string used for the chip/dot. */
  color: string;
  /**
   * The JMAP keyword token this label maps to (lowercase, no
   * spaces). Stable across renames so re-labelling an email is not
   * required when only the display name changes.
   */
  keyword: string;
}

/** Fields a caller supplies when creating a {@link Label}. */
export interface LabelDraft {
  name: string;
  color: string;
}

/**
 * Out-of-office / vacation auto-reply settings. Persisted as a
 * generated Sieve `vacation` script (RFC 5230) deployed through the
 * existing `internal/sieve` rule store; the client keeps this
 * normalized shape so the editor can round-trip without parsing
 * Sieve.
 */
export interface VacationSettings {
  enabled: boolean;
  subject: string;
  message: string;
  /** ISO date (YYYY-MM-DD) the auto-reply starts, or null for "now". */
  startDate: string | null;
  /** ISO date (YYYY-MM-DD) the auto-reply ends, or null for "until off". */
  endDate: string | null;
  /** When true, only reply to senders in the user's contacts/GAL. */
  contactsOnly: boolean;
}

/**
 * A delegate-access / send-as grant. `delegateEmail` is the user
 * being granted access; `access` is the level of mailbox access
 * (RFC 8621 mailbox rights are coarser, so we model the product
 * concept here). `sendAs` allows the delegate to send messages
 * under the grantor's identity.
 */
export interface DelegationGrant {
  id: string;
  /** The mailbox owner granting access (From identity email). */
  ownerEmail: string;
  /** The user being granted access. */
  delegateEmail: string;
  access: "none" | "read" | "read-write";
  sendAs: boolean;
  createdAt: string;
}

/** Fields a caller supplies when creating a {@link DelegationGrant}. */
export interface DelegationGrantDraft {
  ownerEmail: string;
  delegateEmail: string;
  access: "none" | "read" | "read-write";
  sendAs: boolean;
}
