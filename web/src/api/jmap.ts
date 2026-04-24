import {
  JMAP_MAIL_CAPABILITY,
  JMAP_SUBMISSION_CAPABILITY,
  type Email,
  type EmailDraft,
  type GetEmailsOptions,
  type Identity,
  type JmapInvocation,
  type JmapResponse,
  type JmapResponseInvocation,
  type JmapSession,
  type Mailbox,
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
   */
  async request(methodCalls: JmapInvocation[]): Promise<JmapResponse> {
    const session = await this.getSession();
    const res = await fetch(session.apiUrl, {
      method: "POST",
      credentials: "include",
      headers: authHeaders({
        "Content-Type": "application/json",
        Accept: "application/json",
      }),
      body: JSON.stringify({
        using: [JMAP_MAIL_CAPABILITY, JMAP_SUBMISSION_CAPABILITY],
        methodCalls,
      }),
    });
    if (!res.ok) {
      throw new Error(
        `kmail-web: JMAP request failed: ${res.status} ${res.statusText}`,
      );
    }
    return (await res.json()) as JmapResponse;
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
  ): Promise<string> {
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
    const response = await this.request([
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
    ]);
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
    return created.draft.id;
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
  return create;
}
