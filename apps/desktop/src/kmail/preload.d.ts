// Ambient typings for the `window.kmail` bridge exposed by
// `electron/preload.ts`.
//
// These types must be kept in lockstep with the bridge surface
// in the preload script AND the napi-rs-generated `index.d.ts`
// at `sdk/kmail-napi/index.d.ts`. We re-declare the napi JS
// shapes here rather than importing them from `@kmail/sdk-native`
// because the renderer is forbidden from importing the addon
// (see `vite.config.ts` alias to `sdk-native.block.ts`).
//
// The field naming convention mirrors napi-rs's default
// camelCase output for `#[napi(object)]` records: Rust's
// `bff_url` becomes JS's `bffUrl`. Method names also follow
// camelCase. Any divergence here breaks at compile time via the
// `tsc -p tsconfig.electron.json` step that links these types
// against `electron/preload.ts`.

export interface JsClientConfig {
  bffUrl: string;
  bearerToken: string;
  databasePath: string;
  attachmentCacheBytes?: bigint;
  requestTimeoutSecs?: number;
  retryBudgetSecs?: number;
  initialSyncEmailWindow?: number;
  accountId?: string;
  bootstrapMailboxRole?: string;
}

export interface JsMailbox {
  id: string;
  name: string;
  role?: string;
  parentId?: string;
  sortOrder: number;
  totalEmails: bigint;
  unreadEmails: bigint;
  isVault: boolean;
}

export interface JsEmailAddress {
  name: string;
  email: string;
}

export interface JsEmailSummary {
  id: string;
  threadId: string;
  blobId: string;
  mailboxIds: string[];
  keywordFlags: string[];
  size: bigint;
  receivedAtUnix: number;
  sentAtUnix?: number;
  fromAddresses: JsEmailAddress[];
  toAddresses: JsEmailAddress[];
  ccAddresses: JsEmailAddress[];
  bccAddresses: JsEmailAddress[];
  subject: string;
  preview: string;
  hasAttachment: boolean;
}

export interface JsSyncSummary {
  mailboxesUpserted: bigint;
  mailboxesDestroyed: bigint;
  emailsCreated: bigint;
  emailsUpdated: bigint;
  emailsDestroyed: bigint;
  pendingActionsApplied: bigint;
  pendingActionsFailed: bigint;
  pendingActionsDeferred: bigint;
}

export interface KMailBridge {
  open(config: JsClientConfig): Promise<void>;
  close(): Promise<void>;
  sync(): Promise<JsSyncSummary>;
  setBearerToken(token: string): Promise<void>;
  invalidateSession(): Promise<void>;
  cachedMailboxes(): Promise<JsMailbox[]>;
  cachedEmails(mailboxId: string, limit: number): Promise<JsEmailSummary[]>;
  sendEmail(draftJson: string): Promise<string>;
  enqueueSetKeywords(emailId: string, keywordsJson: string): Promise<void>;
  notify(title: string, body: string): Promise<void>;
}

declare global {
  interface Window {
    kmail: KMailBridge;
  }
}

export {};
