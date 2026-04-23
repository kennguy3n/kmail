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
