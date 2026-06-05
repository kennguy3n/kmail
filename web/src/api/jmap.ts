import {
  JMAP_CALENDARS_CAPABILITY,
  JMAP_MAIL_CAPABILITY,
  JMAP_SUBMISSION_CAPABILITY,
  type Calendar,
  type CalendarEvent,
  type CalendarEventDraft,
  type Email,
  type EmailDraft,
  type EventDateRange,
  type EventParticipantResponse,
  type GetEmailsOptions,
  type Identity,
  type JmapInvocation,
  type JmapResponse,
  type JmapResponseInvocation,
  type JmapSession,
  type Mailbox,
  type SearchEmailsOptions,
} from "../types";

/**
 * JMAP client.
 *
 * Every method here speaks to the Go BFF, not to Stalwart directly.
 * The BFF enforces tenant policy, capability gating, rate limiting,
 * and error mapping — see docs/JMAP-CONTRACT.md for the contract
 * this file implements against.
 */

/** Base URL for all BFF-owned endpoints. */
export const JMAP_BASE_URL = "/jmap";

/** Well-known session discovery URL (RFC 8620 §2.2). */
export const JMAP_SESSION_URL = "/jmap/session";

/**
 * The conventional name for the lazily-provisioned per-user
 * Snoozed mailbox. Exported so both `Inbox.tsx` and
 * `MessageView.tsx` agree on the same lookup-and-create target —
 * neither view should embed its own copy of this string.
 */
export const SNOOZED_MAILBOX_NAME = "Snoozed";

/**
 * Dev-bypass bearer token. The Go BFF's OIDC middleware accepts a
 * static token when `KMAIL_DEV_BYPASS_TOKEN` matches — in local dev
 * we run the stack with `KMAIL_DEV_BYPASS_TOKEN=kmail-dev`, so the
 * React client sends `Authorization: Bearer kmail-dev` on every
 * JMAP request. In staging / production the middleware rejects
 * this value and clients must obtain a real KChat OIDC token; see
 * docs/JMAP-CONTRACT.md §3.1.
 */
export const DEV_BEARER_TOKEN = "kmail-dev";

/**
 * Build the base headers every JMAP request needs. Centralised so
 * the auth wiring only lives in one place — switching from
 * dev-bypass to real OIDC is a single-point edit when that work
 * lands in Phase 3.
 */
function authHeaders(extra: HeadersInit = {}): Headers {
  const h = new Headers(extra);
  h.set("Authorization", `Bearer ${DEV_BEARER_TOKEN}`);
  return h;
}

/**
 * Fetch the JMAP session object. Kept as a standalone helper so
 * tests and the React `useSession` hook can call it without first
 * instantiating a `JMAPClient`.
 */
export async function fetchSession(): Promise<JmapSession> {
  const res = await fetch(JMAP_SESSION_URL, {
    credentials: "include",
    headers: authHeaders({ Accept: "application/json" }),
  });
  if (!res.ok) {
    throw new Error(
      `kmail-web: fetchSession failed: ${res.status} ${res.statusText}`,
    );
  }
  return (await res.json()) as JmapSession;
}

/**
 * Thrown when the BFF returns a method-level error inside an
 * otherwise-successful batch response. Carries the JMAP
 * `methodResponses` entry so callers can inspect the error type
 * and description.
 */
export class JmapMethodError extends Error {
  readonly method: string;
  readonly callId: string;
  readonly result: Record<string, unknown>;
  constructor(invocation: JmapResponseInvocation) {
    const [method, result, callId] = invocation;
    const type = typeof result.type === "string" ? result.type : "unknown";
    const description =
      typeof result.description === "string"
        ? `: ${result.description}`
        : "";
    super(`JMAP ${method} error: ${type}${description}`);
    this.name = "JmapMethodError";
    this.method = method;
    this.callId = callId;
    this.result = result;
  }
}

/**
 * Typed JMAP client. One instance per browser session is enough;
 * the client lazily fetches the session document on the first call
 * and caches it thereafter.
 */
export class JMAPClient {
  private session: JmapSession | null = null;
  private defaultIdentityId: string | null = null;

  /**
   * Return a cached session or fetch and cache it. Callers rarely
   * need to interact with the session directly — the typed methods
   * below pick the right accountId and apiUrl automatically — but
   * exposing this is convenient for settings / debug surfaces.
   */
  async getSession(): Promise<JmapSession> {
    if (this.session === null) {
      this.session = await fetchSession();
    }
    return this.session;
  }

  /**
   * Clear the cached session. Called by the login/logout flow so a
   * new user does not inherit the previous tenant's accountId or
   * default identity.
   */
  resetSession(): void {
    this.session = null;
    this.defaultIdentityId = null;
  }

  /**
   * Return the primary Mail accountId for the current session. The
   * BFF guarantees exactly one Mail account per user in Phase 2, so
   * we pick it from `primaryAccounts[urn:ietf:params:jmap:mail]`.
   */
  async getAccountId(): Promise<string> {
    const session = await this.getSession();
    const accountId = session.primaryAccounts[JMAP_MAIL_CAPABILITY];
    if (!accountId) {
      throw new Error(
        "kmail-web: session has no primary Mail account",
      );
    }
    return accountId;
  }

  /**
   * Send a batch of JMAP invocations to the BFF and return the raw
   * response envelope. Typed helpers (`getMailboxes`, `getEmails`,
   * etc.) call this under the hood; callers that need a spec-level
   * method not yet wrapped in a typed helper can use `request`
   * directly.
   *
   * `using` defaults to the Mail + Submission capabilities so every
   * existing call site keeps working; calendar helpers pass the
   * Core + Calendars capabilities instead so Stalwart resolves the
   * `CalendarEvent/*` methods.
   */
  async request(
    methodCalls: JmapInvocation[],
    using: string[] = [JMAP_MAIL_CAPABILITY, JMAP_SUBMISSION_CAPABILITY],
  ): Promise<JmapResponse> {
    const { body } = await this.requestWithHeaders(methodCalls, using);
    return body;
  }

  /**
   * Lower-level variant of `request` that returns both the parsed
   * JMAP response envelope AND the underlying HTTP response
   * headers. The Undo-Send proxy hook stamps
   * `X-KMail-Pending-Send-Id` / `X-KMail-Undo-Deadline` on the
   * EmailSubmission/set response; this method is the seam Compose
   * reads those values through.
   */
  async requestWithHeaders(
    methodCalls: JmapInvocation[],
    using: string[] = [JMAP_MAIL_CAPABILITY, JMAP_SUBMISSION_CAPABILITY],
    extraHeaders: Record<string, string> = {},
  ): Promise<{ body: JmapResponse; headers: Headers }> {
    const session = await this.getSession();
    const res = await fetch(session.apiUrl, {
      method: "POST",
      credentials: "include",
      headers: authHeaders({
        "Content-Type": "application/json",
        Accept: "application/json",
        ...extraHeaders,
      }),
      body: JSON.stringify({
        using,
        methodCalls,
      }),
    });
    if (!res.ok) {
      throw new Error(
        `kmail-web: JMAP request failed: ${res.status} ${res.statusText}`,
      );
    }
    const body = (await res.json()) as JmapResponse;
    return { body, headers: res.headers };
  }

  /** Fetch every mailbox for the current account. */
  async getMailboxes(): Promise<Mailbox[]> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      ["Mailbox/get", { accountId, ids: null }, "0"],
    ]);
    const result = expectResult(response, "Mailbox/get", "0");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Mailbox/get returned no list");
    }
    return list as Mailbox[];
  }

  /**
   * Fetch a page of emails from `mailboxId`. Uses Email/query to
   * resolve IDs under the caller's sort/limit, then Email/get to
   * hydrate each row with the fields the list view needs. Result
   * bodies are requested via `properties` rather than `bodyValues`
   * so the payload stays small; use `getEmail(id)` to fetch the
   * full body for a selected message.
   */
  async getEmails(
    mailboxId: string,
    options: GetEmailsOptions = {},
  ): Promise<Email[]> {
    const accountId = await this.getAccountId();
    const {
      limit = 50,
      position = 0,
      sort = [{ property: "receivedAt", isAscending: false }],
    } = options;
    const response = await this.request([
      [
        "Email/query",
        {
          accountId,
          filter: { inMailbox: mailboxId },
          sort,
          position,
          limit,
          calculateTotal: true,
        },
        "0",
      ],
      [
        "Email/get",
        {
          accountId,
          "#ids": {
            resultOf: "0",
            name: "Email/query",
            path: "/ids",
          },
          properties: [
            "id",
            "threadId",
            "mailboxIds",
            "keywords",
            "from",
            "to",
            "subject",
            "receivedAt",
            "sentAt",
            "size",
            "preview",
            "hasAttachment",
            "privacyMode",
          ],
        },
        "1",
      ],
    ]);
    const result = expectResult(response, "Email/get", "1");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Email/get returned no list");
    }
    return list as Email[];
  }

  /**
   * Run a full-text search against Stalwart's FTS backend
   * (Meilisearch in Phase 2 — see docs/PROGRESS.md) and hydrate
   * the matching emails.
   *
   * Maps to JMAP `Email/query` with an RFC 8621 §4.4.1
   * `FilterCondition.text` term, optionally `AND`-ed with an
   * `inMailbox` condition when the caller scopes the search to a
   * specific mailbox. The subsequent `Email/get` uses a
   * back-reference (`#ids`) so the two calls share the query
   * result in a single round-trip, matching the pattern used by
   * `getEmails()`.
   *
   * Scope:
   *
   *   - `opts.mailboxId == null` (the default) → global search
   *     across every mailbox the authenticated user can see. Vault
   *     mailboxes are filtered out server-side per
   *     docs/JMAP-CONTRACT.md §2.4.
   *   - `opts.mailboxId == "..."` → scoped search; the BFF
   *     combines `inMailbox` and `text` into a single
   *     `FilterOperator.AND`.
   *
   * An empty query string returns no results without issuing a
   * network request — Stalwart would otherwise interpret it as an
   * unbounded search.
   */
  async searchEmails(
    query: string,
    opts: SearchEmailsOptions = {},
  ): Promise<Email[]> {
    const trimmed = query.trim();
    if (trimmed.length === 0) return [];
    const accountId = await this.getAccountId();
    const {
      limit = 50,
      position = 0,
      mailboxId = null,
      sort = [{ property: "receivedAt", isAscending: false }],
    } = opts;
    const filter: Record<string, unknown> = mailboxId
      ? {
          operator: "AND",
          conditions: [
            { inMailbox: mailboxId },
            { text: trimmed },
          ],
        }
      : { text: trimmed };
    const response = await this.request([
      [
        "Email/query",
        {
          accountId,
          filter,
          sort,
          position,
          limit,
          calculateTotal: true,
        },
        "0",
      ],
      [
        "Email/get",
        {
          accountId,
          "#ids": {
            resultOf: "0",
            name: "Email/query",
            path: "/ids",
          },
          properties: [
            "id",
            "threadId",
            "mailboxIds",
            "keywords",
            "from",
            "to",
            "subject",
            "receivedAt",
            "sentAt",
            "size",
            "preview",
            "hasAttachment",
            "privacyMode",
          ],
        },
        "1",
      ],
    ]);
    const result = expectResult(response, "Email/get", "1");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Email/get returned no list");
    }
    return list as Email[];
  }

  /**
   * Fetch a single email with its full body. Requests both text
   * and html bodies so the message view can prefer html and fall
   * back to text without a second round-trip.
   */
  async getEmail(emailId: string): Promise<Email> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      [
        "Email/get",
        {
          accountId,
          ids: [emailId],
          properties: [
            "id",
            "blobId",
            "threadId",
            "mailboxIds",
            "keywords",
            "size",
            "from",
            "to",
            "cc",
            "bcc",
            "replyTo",
            "subject",
            "receivedAt",
            "sentAt",
            "hasAttachment",
            "preview",
            "textBody",
            "htmlBody",
            "attachments",
            "bodyValues",
            "privacyMode",
            // Read-receipt (RFC 8098) request headers + Message-ID,
            // surfaced so MessageView can offer to send an MDN.
            "header:Disposition-Notification-To:asText",
            "header:Message-ID:asText",
          ],
          fetchTextBodyValues: true,
          fetchHTMLBodyValues: true,
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "Email/get", "0");
    const list = result.list;
    if (!Array.isArray(list) || list.length === 0) {
      throw new Error(`kmail-web: email ${emailId} not found`);
    }
    return list[0] as Email;
  }

  /**
   * Send a Message Disposition Notification (RFC 8098) for an email
   * the user just read, to the address the sender named in the
   * `Disposition-Notification-To` header.
   *
   * The MDN is a `multipart/report; report-type=disposition-notification`
   * message with two parts: a human-readable explanation and the
   * machine-readable `message/disposition-notification` part. We
   * build the structure explicitly (rather than via
   * `buildEmailCreate`, which only emits text/html bodies) and
   * submit it in the same `Email/set` + `EmailSubmission/set`
   * round-trip used by `sendEmail`.
   */
  async sendReadReceipt(params: {
    to: string;
    fromIdentity: Identity;
    originalMessageId: string | null;
    originalSubject: string | null;
    originalRecipient: string;
  }): Promise<void> {
    const accountId = await this.getAccountId();
    const { to, fromIdentity, originalMessageId, originalSubject } = params;
    const human =
      `Your message${originalSubject ? ` "${originalSubject}"` : ""} ` +
      `was displayed by ${params.originalRecipient}. This is an automatic ` +
      `read-receipt confirmation; no further action is required.`;
    // RFC 8098 §3.1 fields. `manual-action/MDN-sent-manually` reflects
    // that the user explicitly chose to send the receipt.
    const mdnFields =
      `Reporting-UA: kmail; KMail Web\r\n` +
      `Final-Recipient: rfc822; ${params.originalRecipient}\r\n` +
      (originalMessageId
        ? `Original-Message-ID: ${originalMessageId}\r\n`
        : "") +
      `Disposition: manual-action/MDN-sent-manually; displayed\r\n`;

    const create: Record<string, unknown> = {
      mailboxIds: {},
      from: [{ name: fromIdentity.name || null, email: fromIdentity.email }],
      to: [{ name: null, email: to }],
      subject: `Read: ${originalSubject ?? "(no subject)"}`,
      "header:Content-Type:asText":
        'multipart/report; report-type=disposition-notification',
      bodyStructure: {
        type: "multipart/report",
        subParts: [
          { partId: "human", type: "text/plain" },
          { partId: "mdn", type: "message/disposition-notification" },
        ],
      },
      bodyValues: {
        human: { value: human },
        mdn: { value: mdnFields },
      },
    };

    const response = await this.request([
      [
        "Email/set",
        { accountId, create: { mdn: create } },
        "0",
      ],
      [
        "EmailSubmission/set",
        {
          accountId,
          create: {
            sub: {
              identityId: fromIdentity.id,
              emailId: "#mdn",
            },
          },
          onSuccessDestroyEmail: ["#sub"],
        },
        "1",
      ],
    ]);
    const setResult = expectResult(response, "Email/set", "0");
    const notCreated = setResult.notCreated as
      | Record<string, { type?: string; description?: string }>
      | undefined;
    if (notCreated && notCreated.mdn) {
      const e = notCreated.mdn;
      throw new Error(
        `kmail-web: failed to build read receipt: ${e.type ?? "unknown"}${e.description ? `: ${e.description}` : ""}`,
      );
    }
    const subResult = expectResult(response, "EmailSubmission/set", "1");
    const subNotCreated = subResult.notCreated as
      | Record<string, { type?: string; description?: string }>
      | undefined;
    if (subNotCreated && subNotCreated.sub) {
      const e = subNotCreated.sub;
      throw new Error(
        `kmail-web: failed to send read receipt: ${e.type ?? "unknown"}${e.description ? `: ${e.description}` : ""}`,
      );
    }
  }

  /**
   * Fetch every Email in a thread, hydrated with the full body
   * property set, in the server's conversation order (RFC 8621
   * §3.2 orders `Thread.emailIds` by receivedAt ascending). Used by
   * the conversation view to render every message in a thread.
   *
   * Resolves `Thread/get` → `Email/get` in a single batch via a
   * back-reference (`#ids`) so the two calls share one round-trip,
   * matching the pattern used by `getEmails()`.
   */
  async getThread(threadId: string): Promise<Email[]> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      ["Thread/get", { accountId, ids: [threadId] }, "0"],
      [
        "Email/get",
        {
          accountId,
          "#ids": {
            resultOf: "0",
            name: "Thread/get",
            path: "/list/*/emailIds",
          },
          properties: [
            "id",
            "blobId",
            "threadId",
            "mailboxIds",
            "keywords",
            "size",
            "from",
            "to",
            "cc",
            "bcc",
            "replyTo",
            "subject",
            "receivedAt",
            "sentAt",
            "hasAttachment",
            "preview",
            "textBody",
            "htmlBody",
            "attachments",
            "bodyValues",
            "privacyMode",
          ],
          fetchTextBodyValues: true,
          fetchHTMLBodyValues: true,
        },
        "1",
      ],
    ]);
    const result = expectResult(response, "Email/get", "1");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Thread Email/get returned no list");
    }
    return list as Email[];
  }

  /**
   * Fetch a page of emails carrying `keyword` (used by the label
   * filter in the sidebar). Mirrors `getEmails()` but filters on
   * `hasKeyword` (RFC 8621 §4.4.1) instead of `inMailbox`.
   */
  async getEmailsByKeyword(
    keyword: string,
    options: GetEmailsOptions = {},
  ): Promise<Email[]> {
    const accountId = await this.getAccountId();
    const {
      limit = 50,
      position = 0,
      sort = [{ property: "receivedAt", isAscending: false }],
    } = options;
    const response = await this.request([
      [
        "Email/query",
        {
          accountId,
          filter: { hasKeyword: keyword },
          sort,
          position,
          limit,
          calculateTotal: true,
        },
        "0",
      ],
      [
        "Email/get",
        {
          accountId,
          "#ids": { resultOf: "0", name: "Email/query", path: "/ids" },
          properties: [
            "id",
            "threadId",
            "mailboxIds",
            "keywords",
            "from",
            "to",
            "subject",
            "receivedAt",
            "sentAt",
            "size",
            "preview",
            "hasAttachment",
            "privacyMode",
          ],
        },
        "1",
      ],
    ]);
    const result = expectResult(response, "Email/get", "1");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Email/get returned no list");
    }
    return list as Email[];
  }

  /**
   * Toggle a single keyword (RFC 8621 §4.1.1) on one email. Used to
   * apply/remove a label. `on=false` clears the keyword by setting
   * it to `null` (JMAP patch semantics).
   */
  async setKeyword(
    emailId: string,
    keyword: string,
    on: boolean,
  ): Promise<void> {
    await this.bulkSetKeyword([emailId], keyword, on);
  }

  /**
   * Set or clear a keyword on many emails in a single `Email/set`
   * batch. Used by bulk operations and the drag-onto-label flow.
   * Throws if the server reports any id under `notUpdated`.
   */
  async bulkSetKeyword(
    emailIds: string[],
    keyword: string,
    on: boolean,
  ): Promise<void> {
    if (emailIds.length === 0) return;
    const accountId = await this.getAccountId();
    const update: Record<string, Record<string, boolean | null>> = {};
    for (const id of emailIds) {
      update[id] = { [`keywords/${keyword}`]: on ? true : null };
    }
    const response = await this.request([
      ["Email/set", { accountId, update }, "0"],
    ]);
    assertAllUpdated(response, emailIds, `set keyword ${keyword}`);
  }

  /**
   * Mark many emails read/unread in a single batch. Mirrors
   * `markRead` for the bulk toolbar.
   */
  async bulkSetSeen(emailIds: string[], read: boolean): Promise<void> {
    if (emailIds.length === 0) return;
    const accountId = await this.getAccountId();
    const update: Record<string, Record<string, boolean | null>> = {};
    for (const id of emailIds) {
      update[id] = { "keywords/$seen": read ? true : null };
    }
    const response = await this.request([
      ["Email/set", { accountId, update }, "0"],
    ]);
    assertAllUpdated(response, emailIds, "set $seen");
  }

  /**
   * Move many emails to `toMailbox` in a single batch. When
   * `fromMailbox` is given, each email is removed from it; otherwise
   * the email keeps any other mailbox memberships (additive move,
   * used by "Archive"/"Apply"). Mirrors `moveEmail`.
   */
  async bulkMove(
    emailIds: string[],
    fromMailbox: string | null,
    toMailbox: string,
  ): Promise<void> {
    if (emailIds.length === 0) return;
    const accountId = await this.getAccountId();
    const update: Record<string, Record<string, boolean | null>> = {};
    for (const id of emailIds) {
      const patch: Record<string, boolean | null> = {
        [`mailboxIds/${toMailbox}`]: true,
      };
      if (fromMailbox) patch[`mailboxIds/${fromMailbox}`] = null;
      update[id] = patch;
    }
    const response = await this.request([
      ["Email/set", { accountId, update }, "0"],
    ]);
    assertAllUpdated(response, emailIds, "move");
  }

  /** Destroy many emails in a single batch (bulk delete). */
  async bulkDelete(emailIds: string[]): Promise<void> {
    if (emailIds.length === 0) return;
    const accountId = await this.getAccountId();
    const response = await this.request([
      ["Email/set", { accountId, destroy: emailIds }, "0"],
    ]);
    const result = expectResult(response, "Email/set", "0");
    const destroyed = (result.destroyed as string[] | undefined) ?? [];
    const missing = emailIds.filter((id) => !destroyed.includes(id));
    if (missing.length > 0) {
      throw new Error(
        `kmail-web: ${missing.length} email(s) were not destroyed`,
      );
    }
  }

  /**
   * Upload a binary blob to the JMAP blob store (RFC 8620 §6.1) and
   * return the server-assigned blob id. Used for inline-image paste
   * and real MIME attachments in Compose. The `uploadUrl` template
   * carries an `{accountId}` placeholder per the session object.
   */
  async uploadBlob(
    file: Blob,
    filename?: string,
  ): Promise<{ blobId: string; type: string; size: number }> {
    const session = await this.getSession();
    const accountId = await this.getAccountId();
    const url = session.uploadUrl.replace(
      "{accountId}",
      encodeURIComponent(accountId),
    );
    // RFC 6266 quoted-string: strip control characters (which would
    // otherwise corrupt or inject HTTP headers) and escape backslashes
    // and double quotes so a filename like `a"b.txt` can't terminate
    // the quoted value early.
    const safeFilename = filename
      ?.replace(/[\u0000-\u001f\u007f]/g, "")
      .replace(/\\/g, "\\\\")
      .replace(/"/g, '\\"');
    const res = await fetch(url, {
      method: "POST",
      credentials: "include",
      headers: authHeaders({
        "Content-Type": file.type || "application/octet-stream",
        ...(safeFilename
          ? { "Content-Disposition": `attachment; filename="${safeFilename}"` }
          : {}),
      }),
      body: file,
    });
    if (!res.ok) {
      throw new Error(
        `kmail-web: blob upload failed: ${res.status} ${res.statusText}`,
      );
    }
    const body = (await res.json()) as {
      blobId: string;
      type?: string;
      size?: number;
    };
    return {
      blobId: body.blobId,
      type: body.type ?? file.type ?? "application/octet-stream",
      size: body.size ?? (file instanceof File ? file.size : 0),
    };
  }

  /**
   * Build a download URL for a blob from the session `downloadUrl`
   * template (RFC 8620 §6.2). Used to render inline images and to
   * preview/download attachments. The returned URL still needs the
   * `Authorization` header, so callers that embed it in an `<img>`
   * tag should fetch via {@link downloadBlob} and use an object URL.
   */
  async buildDownloadUrl(
    blobId: string,
    name: string,
    type = "application/octet-stream",
  ): Promise<string> {
    const session = await this.getSession();
    const accountId = await this.getAccountId();
    return session.downloadUrl
      .replace("{accountId}", encodeURIComponent(accountId))
      .replace("{blobId}", encodeURIComponent(blobId))
      .replace("{name}", encodeURIComponent(name))
      .replace("{type}", encodeURIComponent(type));
  }

  /**
   * Fetch a blob's bytes with the auth header attached. Returns a
   * `Blob` the caller can turn into an object URL for inline image
   * rendering or a download anchor.
   */
  async downloadBlob(
    blobId: string,
    name: string,
    type = "application/octet-stream",
  ): Promise<Blob> {
    const url = await this.buildDownloadUrl(blobId, name, type);
    const res = await fetch(url, {
      credentials: "include",
      headers: authHeaders(),
    });
    if (!res.ok) {
      throw new Error(
        `kmail-web: blob download failed: ${res.status} ${res.statusText}`,
      );
    }
    return res.blob();
  }

  /**
   * Fetch every Identity the authenticated user may send under.
   * Matches RFC 8621 §6.3 (`Identity/get` with `ids: null`).
   */
  async getIdentities(): Promise<Identity[]> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      ["Identity/get", { accountId, ids: null }, "0"],
    ]);
    const result = expectResult(response, "Identity/get", "0");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Identity/get returned no list");
    }
    return list as Identity[];
  }

  /**
   * Resolve an Identity id to send `draft` under. Prefers
   * `draft.identityId` when the caller supplied one; otherwise
   * returns the cached default, fetching and caching it if needed.
   * The default identity is the first entry returned by
   * `Identity/get` — Stalwart orders the list so the account's
   * primary address comes first. Throws if no identities are
   * available for the account.
   */
  private async resolveIdentityId(draft: EmailDraft): Promise<string> {
    if (draft.identityId) return draft.identityId;
    if (this.defaultIdentityId !== null) return this.defaultIdentityId;
    const identities = await this.getIdentities();
    if (identities.length === 0) {
      throw new Error(
        "kmail-web: account has no send-capable identity; set draft.identityId explicitly",
      );
    }
    this.defaultIdentityId = identities[0].id;
    return this.defaultIdentityId;
  }

  /**
   * Create a draft and submit it. Uses a create-ref (`#emailId`)
   * so the Submission happens in the same round-trip as the create,
   * matching the RFC 8621 §7 example for "send in one request".
   *
   * The EmailSubmission result is checked explicitly — RFC 8621
   * §7.5 lets `create` fail per-object (`notCreated`) even when the
   * batch itself succeeds, so a silent `notCreated` entry would
   * otherwise leave the draft sitting in the mailbox forever while
   * the caller believed the email had been sent.
   */
  async sendEmail(
    draft: EmailDraft,
    existingDraftId: string | null = null,
    options: SendEmailOptions = {},
  ): Promise<SendEmailResult> {
    const accountId = await this.getAccountId();
    const identityId = await this.resolveIdentityId(draft);
    const create = buildEmailCreate(draft);
    const emailSetArgs: Record<string, unknown> = {
      accountId,
      create: { draft: create },
    };
    // If the user has already clicked Save draft in this compose
    // session, destroy that stale draft in the same Email/set call
    // so it doesn't linger in the Drafts mailbox after a successful
    // Send. The server-side draft we submit and auto-destroy via
    // `onSuccessDestroyEmail` below is a *different* email (the one
    // this call creates) — without this destroy the prior saved
    // draft would be orphaned.
    if (existingDraftId) {
      emailSetArgs.destroy = [existingDraftId];
    }
    const extraHeaders: Record<string, string> = {};
    if (options.scheduleAt) {
      // Opt in to the BFF Scheduled Send hold queue
      // (internal/scheduledsend). The proxy forwards only the
      // Email/set portion of this batch to Stalwart, persists
      // the EmailSubmission/set payload to Postgres with the
      // target send time, and stamps the row id + send-at on
      // the response headers. We send unix-seconds (smaller and
      // unambiguous) — the BFF also accepts RFC3339 for
      // cURL/Postman callers.
      //
      // When both `scheduleAt` and `undoSend` are set the
      // schedule wins: there is no UX reason to layer a 10s
      // undo banner on top of a "send tomorrow" hold.
      extraHeaders["X-KMail-Schedule-At"] = String(
        Math.floor(options.scheduleAt.getTime() / 1000),
      );
    } else if (options.undoSend) {
      // Opt in to the BFF Undo-Send hold queue (internal/undosend).
      // The proxy will forward only the Email/set portion of this
      // batch to Stalwart, hold the EmailSubmission/set payload in
      // Valkey, and respond with X-KMail-Pending-Send-Id headers.
      extraHeaders["X-KMail-Undo-Send"] = "true";
    }
    const { body: response, headers: responseHeaders } =
      await this.requestWithHeaders(
        [
          ["Email/set", emailSetArgs, "0"],
          [
            "EmailSubmission/set",
            {
              accountId,
              create: {
                submission: {
                  emailId: "#draft",
                  identityId,
                },
              },
              onSuccessDestroyEmail: ["#submission"],
            },
            "1",
          ],
        ],
        undefined,
        extraHeaders,
      );
    const emailResult = expectResult(response, "Email/set", "0");
    const created = emailResult.created as
      | Record<string, { id: string }>
      | null;
    const notCreated = emailResult.notCreated as
      | Record<string, { type: string; description?: string }>
      | undefined;
    if (notCreated && notCreated.draft) {
      const entry = notCreated.draft;
      throw new Error(
        `kmail-web: failed to create draft: ${entry.type}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
    if (!created || !created.draft) {
      throw new Error("kmail-web: sendEmail did not create a draft");
    }
    const submissionResult = expectResult(
      response,
      "EmailSubmission/set",
      "1",
    );
    const submissionNotCreated = submissionResult.notCreated as
      | Record<string, { type: string; description?: string }>
      | undefined;
    if (submissionNotCreated && submissionNotCreated.submission) {
      const entry = submissionNotCreated.submission;
      throw new Error(
        `kmail-web: failed to submit email: ${entry.type}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
    const submissionCreated = submissionResult.created as
      | Record<string, { id: string }>
      | null;
    if (!submissionCreated || !submissionCreated.submission) {
      throw new Error(
        "kmail-web: sendEmail did not create an EmailSubmission",
      );
    }
    // Undo-Send: when the BFF held the submission, the response
    // carries the pending-send id + deadline. Surface them on the
    // result so Compose can render the cancel banner.
    const pendingSendId =
      responseHeaders.get("X-KMail-Pending-Send-Id") ?? null;
    const deadlineHeader =
      responseHeaders.get("X-KMail-Undo-Deadline") ?? null;
    const undoDeadline = deadlineHeader
      ? parseDeadlineHeader(deadlineHeader)
      : null;
    // Scheduled-Send: when the BFF held the submission until a
    // future `send_at`, the response carries the row id and the
    // resolved send-at. Surface both so Compose can show a
    // "Scheduled for X" confirmation and the Scheduled Sends
    // page can link back.
    const scheduledSendId =
      responseHeaders.get("X-KMail-Scheduled-Send-Id") ?? null;
    const scheduleAtHeader =
      responseHeaders.get("X-KMail-Scheduled-Send-At") ?? null;
    const scheduledSendAt = scheduleAtHeader
      ? parseDeadlineHeader(scheduleAtHeader)
      : null;
    return {
      emailId: created.draft.id,
      pendingSendId,
      undoDeadline,
      scheduledSendId,
      scheduledSendAt,
    };
  }

  /**
   * Mark an email as read (`$seen` set) or unread (`$seen` cleared).
   * Uses a JMAP patch path on `keywords/$seen` so we don't need to
   * fetch the current keyword set first. RFC 8621 §4.1.1 defines
   * `$seen` as the canonical read flag.
   */
  async markRead(emailId: string, read: boolean): Promise<void> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      [
        "Email/set",
        {
          accountId,
          update: {
            [emailId]: {
              "keywords/$seen": read ? true : null,
            },
          },
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "Email/set", "0");
    const notUpdated = result.notUpdated as
      | Record<string, unknown>
      | undefined;
    if (notUpdated && notUpdated[emailId]) {
      throw new Error(
        `kmail-web: failed to mark email ${emailId}: ${JSON.stringify(notUpdated[emailId])}`,
      );
    }
  }

  /**
   * Create a draft without submitting it. Used by the compose page
   * for "Save as draft" flows and as the building block for
   * `sendEmail` (which creates a draft and submits it in the same
   * round-trip). Returns the server-assigned draft id.
   */
  async createDraft(draft: EmailDraft): Promise<string> {
    return this.saveDraft(draft, null);
  }

  /**
   * Save a draft, optionally replacing one previously saved in the
   * same compose session. When `existingId` is non-null we batch a
   * `destroy` of the old draft with the `create` of the new one in
   * a single `Email/set` call so the Drafts mailbox never contains
   * two copies of the same in-progress message. The BFF sees this
   * as one atomic change — if the destroy fails (e.g. the user
   * already deleted the old draft from another tab) we still
   * surface the new draft's id.
   */
  async saveDraft(
    draft: EmailDraft,
    existingId: string | null,
  ): Promise<string> {
    const accountId = await this.getAccountId();
    const create = buildEmailCreate(draft);
    const setArgs: Record<string, unknown> = {
      accountId,
      create: { draft: create },
    };
    if (existingId) {
      setArgs.destroy = [existingId];
    }
    const response = await this.request([
      ["Email/set", setArgs, "0"],
    ]);
    const result = expectResult(response, "Email/set", "0");
    const created = result.created as
      | Record<string, { id: string }>
      | null;
    const notCreated = result.notCreated as
      | Record<string, { type: string; description?: string }>
      | undefined;
    if (notCreated && notCreated.draft) {
      const entry = notCreated.draft;
      throw new Error(
        `kmail-web: failed to create draft: ${entry.type}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
    if (!created || !created.draft) {
      throw new Error("kmail-web: createDraft did not return an id");
    }
    return created.draft.id;
  }

  /**
   * Move an email between mailboxes by patching the `mailboxIds`
   * map. Uses JMAP patch paths so we don't need to fetch the
   * current mailbox set first.
   */
  async moveEmail(
    emailId: string,
    fromMailbox: string,
    toMailbox: string,
  ): Promise<void> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      [
        "Email/set",
        {
          accountId,
          update: {
            [emailId]: {
              [`mailboxIds/${fromMailbox}`]: null,
              [`mailboxIds/${toMailbox}`]: true,
            },
          },
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "Email/set", "0");
    const notUpdated = result.notUpdated as
      | Record<string, unknown>
      | undefined;
    if (notUpdated && notUpdated[emailId]) {
      throw new Error(
        `kmail-web: failed to move email ${emailId}: ${JSON.stringify(notUpdated[emailId])}`,
      );
    }
  }

  /**
   * Mark an email as spam (move into the Junk mailbox and set the
   * JMAP `$junk` keyword) or mark it as not spam (move back to
   * Inbox and clear `$junk` / set `$notjunk`). Follows the same
   * JMAP patch-path pattern as `moveEmail` so the BFF sees one
   * atomic `Email/set` update.
   *
   * The `$junk` / `$notjunk` keyword flip drives Stalwart's
   * Bayesian spam classifier (see `scripts/stalwart-init.sh` for
   * the `spam-filter.bayes.auto-learn.*` settings that train
   * against these keywords); the mailbox move is what the user
   * actually sees in the UI.
   *
   * Callers pass the source and destination mailbox ids so the
   * method works in both directions (Inbox → Junk and Junk →
   * Inbox) without having to inspect the email's current mailbox
   * set first.
   */
  async markAsSpam(
    emailId: string,
    fromMailbox: string,
    junkMailbox: string,
    isSpam: boolean,
  ): Promise<void> {
    const accountId = await this.getAccountId();
    const keywords: Record<string, boolean | null> = isSpam
      ? { "keywords/$junk": true, "keywords/$notjunk": null }
      : { "keywords/$junk": null, "keywords/$notjunk": true };
    const [src, dst] = isSpam
      ? [fromMailbox, junkMailbox]
      : [junkMailbox, fromMailbox];
    const response = await this.request([
      [
        "Email/set",
        {
          accountId,
          update: {
            [emailId]: {
              [`mailboxIds/${src}`]: null,
              [`mailboxIds/${dst}`]: true,
              ...keywords,
            },
          },
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "Email/set", "0");
    const notUpdated = result.notUpdated as
      | Record<string, unknown>
      | undefined;
    if (notUpdated && notUpdated[emailId]) {
      throw new Error(
        `kmail-web: failed to mark email ${emailId} as ${isSpam ? "spam" : "not spam"}: ${JSON.stringify(notUpdated[emailId])}`,
      );
    }
  }

  /**
   * Permanently destroy an email. Callers that want "move to
   * trash" semantics should use `moveEmail(emailId, mailboxId,
   * trashMailboxId)` instead; this method is reserved for emptying
   * the trash or for messages whose mailbox has already been
   * resolved as the trash mailbox.
   */
  async deleteEmail(emailId: string): Promise<void> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      [
        "Email/set",
        { accountId, destroy: [emailId] },
        "0",
      ],
    ]);
    const result = expectResult(response, "Email/set", "0");
    const destroyed = result.destroyed as string[] | undefined;
    if (!destroyed || !destroyed.includes(emailId)) {
      throw new Error(`kmail-web: email ${emailId} was not destroyed`);
    }
  }

  /**
   * Create a top-level mailbox with the given name. Returns the
   * server-assigned id. Used by the Snooze feature to lazily
   * provision a per-user "Snoozed" mailbox when the user snoozes
   * their first email.
   */
  async createMailbox(name: string): Promise<string> {
    const accountId = await this.getAccountId();
    const response = await this.request([
      [
        "Mailbox/set",
        {
          accountId,
          create: {
            mb: {
              name,
              parentId: null,
            },
          },
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "Mailbox/set", "0");
    const notCreated = result.notCreated as
      | Record<string, { type?: string; description?: string }>
      | undefined;
    if (notCreated && notCreated.mb) {
      const entry = notCreated.mb;
      throw new Error(
        `kmail-web: failed to create mailbox ${name}: ${entry.type ?? "unknown"}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
    const created = result.created as
      | Record<string, { id: string }>
      | undefined;
    if (!created || !created.mb) {
      throw new Error(
        `kmail-web: createMailbox(${name}) did not return an id`,
      );
    }
    return created.mb.id;
  }

  /**
   * Resolve or lazily create the per-user "Snoozed" mailbox and
   * return its id.
   *
   * Centralised here so that `Inbox.tsx` and `MessageView.tsx`
   * cannot drift into their own copies of the lookup-and-create
   * dance. Without this, a user who snoozes from MessageView
   * (which calls `getMailboxes()` + `createMailbox()` on first
   * use) can then navigate to Inbox before the Inbox's React
   * `mailboxes` state has refetched, and trigger a second
   * `createMailbox("Snoozed")` from the Inbox path — which the
   * JMAP server may reject as a duplicate name and surface as a
   * user-visible error for a mailbox that already exists.
   *
   * This helper always fetches the LIVE mailbox list (not cached
   * React state) so the lookup sees any mailbox just created by
   * another view. On `createMailbox` failure it re-fetches and
   * looks up by role/name as a recovery path — a concurrent
   * create from another view/tab will surface as a server-side
   * rejection here, and the recovery re-fetch is how we
   * reconcile to the now-existing mailbox instead of bubbling
   * the duplicate-name error to the user.
   */
  async resolveOrCreateSnoozedMailbox(): Promise<string> {
    const findSnoozed = (list: Mailbox[]): string | null => {
      const byRole = list.find((m) => m.role === "snoozed");
      if (byRole) return byRole.id;
      const byName = list.find(
        (m) =>
          m.name.toLowerCase() === SNOOZED_MAILBOX_NAME.toLowerCase(),
      );
      return byName?.id ?? null;
    };

    const list = await this.getMailboxes();
    const existing = findSnoozed(list);
    if (existing) return existing;

    try {
      return await this.createMailbox(SNOOZED_MAILBOX_NAME);
    } catch (err) {
      // Recovery path: a concurrent create from another view /
      // tab may have raced ahead between our initial getMailboxes
      // and our createMailbox. Re-fetch and look up again before
      // surfacing the error to the caller.
      const list2 = await this.getMailboxes();
      const recovered = findSnoozed(list2);
      if (recovered) return recovered;
      throw err;
    }
  }

  // ----------------------------------------------------------------
  // Calendars (draft `urn:ietf:params:jmap:calendars`).
  //
  // Stalwart v0.16.0 ships a CalDAV store but does not yet
  // advertise a JMAP calendars capability of its own — the Go BFF
  // is expected to surface these objects on top of the CalDAV
  // collections until upstream parity lands. The React client
  // works against the JMAP shapes today and the BFF swaps its
  // backend without a UI change.
  // ----------------------------------------------------------------

  /**
   * Return the primary Calendar accountId for the current session.
   * Falls back to the Mail accountId when the BFF advertises a
   * single unified account per user (the Phase 2 default).
   */
  async getCalendarAccountId(): Promise<string> {
    const session = await this.getSession();
    const accountId =
      session.primaryAccounts[JMAP_CALENDARS_CAPABILITY] ??
      session.primaryAccounts[JMAP_MAIL_CAPABILITY];
    if (!accountId) {
      throw new Error(
        "kmail-web: session has no primary Calendar account",
      );
    }
    return accountId;
  }

  /**
   * Issue a JMAP batch scoped to the Calendars capability. Mirrors
   * `request()` but swaps the default `using` array so the server
   * knows to resolve `Calendar/*` and `CalendarEvent/*` methods.
   */
  private async calendarRequest(
    methodCalls: JmapInvocation[],
  ): Promise<JmapResponse> {
    return this.request(methodCalls, [
      "urn:ietf:params:jmap:core",
      JMAP_CALENDARS_CAPABILITY,
    ]);
  }

  /** Fetch every calendar visible to the authenticated user. */
  async getCalendars(): Promise<Calendar[]> {
    const accountId = await this.getCalendarAccountId();
    const response = await this.calendarRequest([
      ["Calendar/get", { accountId, ids: null }, "0"],
    ]);
    const result = expectResult(response, "Calendar/get", "0");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: Calendar/get returned no list");
    }
    return list as Calendar[];
  }

  /**
   * Fetch events in `range` from one or more calendars. When
   * `calendarId` is a string the filter is scoped to that
   * calendar; when it is an array every listed calendar is
   * OR-ed; when it is null the query spans every calendar
   * visible to the user.
   *
   * Uses `CalendarEvent/query` (`after` / `before` bounds from
   * the draft spec) + a back-referenced `CalendarEvent/get` so
   * the two calls share the query result in one round-trip.
   */
  async getEvents(
    calendarId: string | string[] | null,
    range: EventDateRange,
  ): Promise<CalendarEvent[]> {
    const accountId = await this.getCalendarAccountId();
    const calendarFilter =
      calendarId === null
        ? null
        : Array.isArray(calendarId)
          ? {
              operator: "OR",
              conditions: calendarId.map((id) => ({ inCalendar: id })),
            }
          : { inCalendar: calendarId };
    const rangeFilter = { after: range.start, before: range.end };
    const filter = calendarFilter
      ? { operator: "AND", conditions: [calendarFilter, rangeFilter] }
      : rangeFilter;
    const response = await this.calendarRequest([
      [
        "CalendarEvent/query",
        { accountId, filter, sort: [{ property: "start", isAscending: true }] },
        "0",
      ],
      [
        "CalendarEvent/get",
        {
          accountId,
          "#ids": {
            resultOf: "0",
            name: "CalendarEvent/query",
            path: "/ids",
          },
          properties: [
            "id",
            "calendarId",
            "title",
            "description",
            "start",
            "end",
            "location",
            "participants",
            "status",
            "recurrenceRules",
          ],
        },
        "1",
      ],
    ]);
    const result = expectResult(response, "CalendarEvent/get", "1");
    const list = result.list;
    if (!Array.isArray(list)) {
      throw new Error("kmail-web: CalendarEvent/get returned no list");
    }
    return list as CalendarEvent[];
  }

  /** Fetch a single event with every field populated. */
  async getEvent(eventId: string): Promise<CalendarEvent> {
    const accountId = await this.getCalendarAccountId();
    const response = await this.calendarRequest([
      [
        "CalendarEvent/get",
        {
          accountId,
          ids: [eventId],
          properties: [
            "id",
            "calendarId",
            "title",
            "description",
            "start",
            "end",
            "location",
            "participants",
            "status",
            "recurrenceRules",
          ],
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "CalendarEvent/get", "0");
    const list = result.list;
    if (!Array.isArray(list) || list.length === 0) {
      throw new Error(`kmail-web: event ${eventId} not found`);
    }
    return list[0] as CalendarEvent;
  }

  /** Create a new calendar event and return the server-assigned id. */
  async createEvent(draft: CalendarEventDraft): Promise<string> {
    const accountId = await this.getCalendarAccountId();
    const response = await this.calendarRequest([
      [
        "CalendarEvent/set",
        {
          accountId,
          create: { event: buildEventCreate(draft) },
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "CalendarEvent/set", "0");
    const notCreated = result.notCreated as
      | Record<string, { type: string; description?: string }>
      | undefined;
    if (notCreated && notCreated.event) {
      const entry = notCreated.event;
      throw new Error(
        `kmail-web: failed to create event: ${entry.type}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
    const created = result.created as
      | Record<string, { id: string }>
      | null;
    if (!created || !created.event) {
      throw new Error("kmail-web: createEvent did not return an id");
    }
    return created.event.id;
  }

  /**
   * Patch the fields listed in `changes` on `eventId`. Caller is
   * responsible for providing only the fields that actually change;
   * the BFF rejects no-op updates server-side.
   */
  async updateEvent(
    eventId: string,
    changes: Partial<CalendarEventDraft>,
  ): Promise<void> {
    const accountId = await this.getCalendarAccountId();
    const response = await this.calendarRequest([
      [
        "CalendarEvent/set",
        {
          accountId,
          update: { [eventId]: changes },
        },
        "0",
      ],
    ]);
    const result = expectResult(response, "CalendarEvent/set", "0");
    const notUpdated = result.notUpdated as
      | Record<string, { type: string; description?: string }>
      | undefined;
    if (notUpdated && notUpdated[eventId]) {
      const entry = notUpdated[eventId];
      throw new Error(
        `kmail-web: failed to update event ${eventId}: ${entry.type}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
  }

  /** Destroy a calendar event. */
  async deleteEvent(eventId: string): Promise<void> {
    const accountId = await this.getCalendarAccountId();
    const response = await this.calendarRequest([
      [
        "CalendarEvent/set",
        { accountId, destroy: [eventId] },
        "0",
      ],
    ]);
    const result = expectResult(response, "CalendarEvent/set", "0");
    const destroyed = result.destroyed as string[] | undefined;
    if (!destroyed || !destroyed.includes(eventId)) {
      throw new Error(`kmail-web: event ${eventId} was not destroyed`);
    }
  }

  /**
   * Update the authenticated user's `rsvp` on `eventId`. Matches
   * the draft JMAP calendars "participant response" semantics: the
   * BFF locates the participant row whose email matches the
   * authenticated user and patches `participants/<key>/rsvp` in a
   * single `CalendarEvent/set update`.
   *
   * `participantEmail` pins the participant row to patch; when
   * omitted the BFF assumes the authenticated user. Passing an
   * explicit value lets delegated-access flows (e.g. an EA
   * responding on behalf of a principal) work later without a
   * second API surface.
   */
  async respondToEvent(
    eventId: string,
    response: EventParticipantResponse,
    participantEmail?: string,
  ): Promise<void> {
    const accountId = await this.getCalendarAccountId();
    const patch: Record<string, unknown> = {};
    if (participantEmail) {
      // Patch path uses the participant email as the key per the
      // draft spec; the BFF normalises the key server-side.
      patch[`participants/${participantEmail}/rsvp`] = response;
    } else {
      patch["participants/self/rsvp"] = response;
    }
    const raw = await this.calendarRequest([
      [
        "CalendarEvent/set",
        {
          accountId,
          update: { [eventId]: patch },
        },
        "0",
      ],
    ]);
    const result = expectResult(raw, "CalendarEvent/set", "0");
    const notUpdated = result.notUpdated as
      | Record<string, { type: string; description?: string }>
      | undefined;
    if (notUpdated && notUpdated[eventId]) {
      const entry = notUpdated[eventId];
      throw new Error(
        `kmail-web: failed to RSVP to event ${eventId}: ${entry.type}${entry.description ? `: ${entry.description}` : ""}`,
      );
    }
  }
}

/** Shared singleton. Callers just `import { jmapClient }`. */
export const jmapClient = new JMAPClient();

/**
 * Find the `methodResponses` entry for `(method, callId)` and
 * return its result. Throws `JmapMethodError` if the BFF returned
 * a method-level error for this call, or a generic Error if the
 * call is missing entirely (which would be a BFF bug).
 */
function expectResult(
  response: JmapResponse,
  method: string,
  callId: string,
): Record<string, unknown> {
  for (const invocation of response.methodResponses) {
    if (invocation[2] !== callId) continue;
    if (invocation[0] === "error") {
      throw new JmapMethodError(invocation);
    }
    if (invocation[0] !== method) {
      throw new Error(
        `kmail-web: expected ${method} for call ${callId}, got ${invocation[0]}`,
      );
    }
    return invocation[1];
  }
  throw new Error(
    `kmail-web: no response for ${method} call ${callId}`,
  );
}

/**
 * Assert that an `Email/set` batch updated every id it was asked
 * to. Used by the bulk-operation helpers, which patch many ids in
 * one call — a partial `notUpdated` would otherwise silently drop
 * some emails from the operation.
 */
function assertAllUpdated(
  response: JmapResponse,
  emailIds: string[],
  action: string,
): void {
  const result = expectResult(response, "Email/set", "0");
  const notUpdated = result.notUpdated as
    | Record<string, unknown>
    | undefined;
  if (!notUpdated) return;
  const failed = emailIds.filter((id) => notUpdated[id]);
  if (failed.length > 0) {
    throw new Error(
      `kmail-web: failed to ${action} ${failed.length} email(s): ${JSON.stringify(
        failed.map((id) => notUpdated[id]),
      )}`,
    );
  }
}

/**
 * Convenience alias for callers that still want the functional
 * invoke() signature from the Phase 1 scaffold. Delegates to the
 * singleton client.
 */
export async function invoke(
  invocations: JmapInvocation[],
): Promise<JmapResponse> {
  return jmapClient.request(invocations);
}

/**
 * Build the `create` object for an `Email/set` call from an
 * `EmailDraft`. Shared between `sendEmail` (create draft + submit)
 * and `createDraft` (create only). Honours both text and HTML
 * bodies; when neither is set a zero-length text part is emitted
 * so RFC 8621 §4.1.4 clients don't see a completely bodiless
 * email.
 */
function buildEmailCreate(draft: EmailDraft): Record<string, unknown> {
  const bodyStructure: Record<string, unknown> = {};
  const bodyValues: Record<string, { value: string }> = {};
  if (draft.htmlBody) {
    bodyStructure.htmlBody = [{ partId: "html", type: "text/html" }];
    bodyValues.html = { value: draft.htmlBody };
  }
  if (draft.textBody || !draft.htmlBody) {
    bodyStructure.textBody = [{ partId: "text", type: "text/plain" }];
    bodyValues.text = { value: draft.textBody ?? "" };
  }
  const create: Record<string, unknown> = {
    mailboxIds: draft.mailboxIds,
    keywords: { $draft: true },
    from: draft.from,
    to: draft.to,
    cc: draft.cc,
    bcc: draft.bcc,
    subject: draft.subject,
    bodyValues,
    ...bodyStructure,
  };
  if (draft.privacyMode) {
    create.privacyMode = draft.privacyMode;
  }
  // Read receipt (RFC 8098 §2): the sender requests a Message
  // Disposition Notification by stamping the recipient-visible
  // notification headers. We set the JMAP `header:*:asText`
  // pseudo-properties (RFC 8621 §4.1.3) so Stalwart emits real
  // RFC 5322 headers rather than us hand-rolling the MIME.
  if (draft.readReceiptTo) {
    create["header:Disposition-Notification-To:asText"] = draft.readReceiptTo;
    create["header:Return-Receipt-To:asText"] = draft.readReceiptTo;
  }
  // Real MIME attachments referenced by blob id (RFC 8621 §4.1.4).
  // Inline parts (cid: images) are emitted with `disposition:
  // "inline"` and a `cid` so the HTML body can reference them.
  if (draft.attachments && draft.attachments.length > 0) {
    create.attachments = draft.attachments.map((a) => {
      const part: Record<string, unknown> = {
        blobId: a.blobId,
        type: a.type,
        name: a.name,
        disposition: a.inline ? "inline" : "attachment",
      };
      if (a.cid) part.cid = a.cid;
      if (typeof a.size === "number") part.size = a.size;
      return part;
    });
  }
  return create;
}

/**
 * Build the `create` object for a `CalendarEvent/set` call. The
 * draft spec wire shape is close to the client-side one; this
 * helper exists so optional fields get stripped (the BFF rejects
 * `null` for fields not in the `null`-allowed set).
 */
function buildEventCreate(
  draft: CalendarEventDraft,
): Record<string, unknown> {
  const create: Record<string, unknown> = {
    calendarId: draft.calendarId,
    title: draft.title,
    start: draft.start,
    end: draft.end,
  };
  if (draft.description) create.description = draft.description;
  if (draft.location) create.location = draft.location;
  if (draft.participants && draft.participants.length > 0) {
    create.participants = draft.participants;
  }
  if (draft.status) create.status = draft.status;
  if (draft.recurrenceRules && draft.recurrenceRules.length > 0) {
    create.recurrenceRules = draft.recurrenceRules;
  }
  return create;
}

// ---------------------------------------------------------------
// Undo Send (Phase 9 / WS3)
// ---------------------------------------------------------------

/** Caller-side options for `sendEmail`. */
export interface SendEmailOptions {
  /**
   * When true, the JMAP request carries the `X-KMail-Undo-Send`
   * opt-in header. The BFF proxy hook then holds the
   * `EmailSubmission/set` portion in Valkey for the configured
   * delay (default 10s) instead of forwarding immediately. The
   * response carries the pending-send id + deadline so Compose
   * can render the cancel banner.
   */
  undoSend?: boolean;
  /**
   * When set, the JMAP request carries the
   * `X-KMail-Schedule-At` header. The BFF proxy hook then
   * persists the EmailSubmission/set payload to Postgres until
   * the given timestamp and dispatches via the scheduled-send
   * worker. The response carries the row id and the resolved
   * send-at so Compose can confirm "Scheduled for X".
   *
   * When `scheduleAt` is set, `undoSend` is ignored — there is
   * no UX value in stacking a 10s undo banner on top of a
   * "send tomorrow" hold.
   */
  scheduleAt?: Date;
}

/**
 * Result of a `sendEmail` call.
 *
 * `pendingSendId` and `undoDeadline` are populated only when the
 * Undo-Send hook intercepted the request. For non-undo sends both
 * fields are `null` and the result behaves like the previous
 * `Promise<string>` shape.
 */
export interface SendEmailResult {
  /** The minted Email id (the draft Stalwart created). */
  emailId: string;
  /** Proxy-issued pending-send id, or null when not held. */
  pendingSendId: string | null;
  /** Absolute deadline (Date) past which Cancel is no longer possible. */
  undoDeadline: Date | null;
  /** Proxy-issued scheduled-send id, or null when not scheduled. */
  scheduledSendId: string | null;
  /** Absolute send-at (Date) for a scheduled send, or null. */
  scheduledSendAt: Date | null;
}

/**
 * Parse the `X-KMail-Undo-Deadline` header (unix-seconds-int) into
 * a `Date`. Returns null for any malformed value rather than
 * throwing — the Compose page still needs to fall back to "Message
 * sent" even when the deadline header is missing/garbage.
 */
function parseDeadlineHeader(raw: string): Date | null {
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n) || n <= 0) return null;
  return new Date(n * 1000);
}

// ---------------------------------------------------------------
// Attachment-to-link (Phase 3, attachments > 10 MB)
// ---------------------------------------------------------------

export interface AttachmentLinkResponse {
  id: string;
  url: string;
  expiry: string;
  filename: string;
  size_bytes: number;
}

/**
 * Uploads an attachment larger than the JMAP blob-size threshold
 * (default 10 MB) to the BFF, which forwards it to zk-object-fabric
 * and returns a presigned GET URL valid for 7 days.
 *
 * Unlike the JMAP `Upload` endpoint, this path stores metadata in
 * `attachment_links` so the URL can be revoked via
 * `DELETE /api/v1/attachments/{id}`.
 */
export async function uploadLargeAttachment(file: File): Promise<AttachmentLinkResponse> {
  const body = new FormData();
  body.set("file", file, file.name);
  const res = await fetch(`/api/v1/attachments/upload`, {
    method: "POST",
    credentials: "include",
    headers: { Authorization: `Bearer ${DEV_BEARER_TOKEN}` },
    body,
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`attachment upload failed: ${res.status} ${text}`);
  }
  return (await res.json()) as AttachmentLinkResponse;
}

/** Fetches a fresh presigned URL for a previously-uploaded attachment. */
export async function getAttachmentLink(id: string): Promise<AttachmentLinkResponse> {
  const res = await fetch(`/api/v1/attachments/${encodeURIComponent(id)}/link`, {
    method: "GET",
    credentials: "include",
    headers: { Authorization: `Bearer ${DEV_BEARER_TOKEN}` },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`attachment link failed: ${res.status} ${text}`);
  }
  return (await res.json()) as AttachmentLinkResponse;
}

/** Revokes an attachment so the presigned link stops resolving. */
export async function revokeAttachment(id: string): Promise<void> {
  const res = await fetch(`/api/v1/attachments/${encodeURIComponent(id)}`, {
    method: "DELETE",
    credentials: "include",
    headers: { Authorization: `Bearer ${DEV_BEARER_TOKEN}` },
  });
  if (!res.ok && res.status !== 204) {
    const text = await res.text().catch(() => res.statusText);
    throw new Error(`attachment revoke failed: ${res.status} ${text}`);
  }
}

/** Threshold in bytes above which attachments are converted to links. */
export const ATTACHMENT_LINK_THRESHOLD_BYTES = 10 * 1024 * 1024;
