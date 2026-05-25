// Thin typed wrapper around `window.kmail` for renderer code.
//
// Adds:
//   - `KMailError` typed exceptions parsed from the IPC-relayed
//     tag prefix.
//   - Convenience helpers: `sendDraft(draft)` accepts a typed
//     `EmailDraft` object instead of a JSON string.
//   - A test seam: `useKMail(stub)` returns whichever bridge the
//     caller injects, falling back to `window.kmail`. This lets
//     vitest run renderer logic without a real Electron host.

import { parseKMailError, KMailError } from './errors';
import type {
  JsClientConfig,
  JsEmailSummary,
  JsMailbox,
  JsSyncSummary,
  KMailBridge,
} from './preload';

export interface EmailAddress {
  name: string;
  email: string;
}

export interface EmailDraft {
  mailboxIds: Record<string, boolean>;
  from: EmailAddress[];
  to: EmailAddress[];
  cc?: EmailAddress[];
  bcc?: EmailAddress[];
  replyTo?: EmailAddress[];
  subject: string;
  textBody?: string;
  htmlBody?: string;
  inReplyTo?: string[];
  references?: string[];
}

/**
 * Encode an `EmailDraft` to the JMAP wire-format JSON the SDK
 * expects.
 *
 * **Cross-binding parity contract.** The Swift facade at
 * `apps/ios/Sources/KMail/KMail.swift::makeKMailWireFormatJSONEncoder`
 * and the Kotlin facade at
 * `apps/android/kmail-sdk/src/main/kotlin/com/kmail/sdk/KMail.kt::wireFormatJson`
 * both lock in the same property-order and empty-default-emission
 * semantics so the BFF sees byte-identical payloads from every
 * client. The desktop side mirrors that contract here:
 *
 *   - Every optional field is materialised to its declared default
 *     (`[]` for arrays) before encoding, so the BFF observes the
 *     same key set regardless of which client sent the draft.
 *   - Keys are written in the same RFC 8621 order as the Rust
 *     `EmailDraft` struct in `sdk/kmail-core/src/models.rs` — the
 *     Rust deserialiser doesn't care, but ordering helps the
 *     observability dashboards diff payloads cleanly.
 *
 * If you change the field set here, you MUST also change the Swift
 * factory + Kotlin `wireFormatJson` AND the Rust `EmailDraft`
 * struct, or you'll silently break the JMAP `Email/set create`
 * payload on at least one platform.
 */
export function encodeWireFormatDraft(draft: EmailDraft): string {
  const wire = {
    mailboxIds: draft.mailboxIds,
    from: draft.from,
    to: draft.to,
    cc: draft.cc ?? [],
    bcc: draft.bcc ?? [],
    replyTo: draft.replyTo ?? [],
    subject: draft.subject,
    textBody: draft.textBody ?? null,
    htmlBody: draft.htmlBody ?? null,
    inReplyTo: draft.inReplyTo ?? [],
    references: draft.references ?? [],
  };
  return JSON.stringify(wire);
}

export class KMailDesktopClient {
  constructor(private readonly bridge: KMailBridge) {}

  async open(config: JsClientConfig): Promise<void> {
    try {
      await this.bridge.open(config);
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async close(): Promise<void> {
    // The `kmail:close` IPC handler today only nulls the session
    // reference and cannot throw, but we still wrap the call in
    // `parseKMailError` for pattern consistency with every other
    // method on this class. If the main-process handler ever gains
    // teardown logic that could fail (e.g. an explicit SQLite WAL
    // checkpoint or a synchronous JMAP push unsubscribe), the
    // error will surface as a typed `KMailError` for consumers
    // that rely on `instanceof KMailError` checks rather than
    // arriving as an untagged raw `Error`.
    try {
      await this.bridge.close();
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async sync(): Promise<JsSyncSummary> {
    try {
      return await this.bridge.sync();
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async setBearerToken(token: string): Promise<void> {
    try {
      await this.bridge.setBearerToken(token);
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async invalidateSession(): Promise<void> {
    try {
      await this.bridge.invalidateSession();
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async cachedMailboxes(): Promise<JsMailbox[]> {
    try {
      return await this.bridge.cachedMailboxes();
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async cachedEmails(
    mailboxId: string,
    limit: number,
  ): Promise<JsEmailSummary[]> {
    try {
      return await this.bridge.cachedEmails(mailboxId, limit);
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async sendDraft(draft: EmailDraft): Promise<string> {
    try {
      return await this.bridge.sendEmail(encodeWireFormatDraft(draft));
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async setKeywords(
    emailId: string,
    keywords: Record<string, boolean>,
  ): Promise<void> {
    try {
      await this.bridge.enqueueSetKeywords(
        emailId,
        JSON.stringify(keywords),
      );
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  async notify(title: string, body: string): Promise<void> {
    try {
      await this.bridge.notify(title, body);
    } catch (err) {
      throw parseKMailError(err);
    }
  }

  /**
   * Fetch the SDK's canonical default config, sourced from the
   * napi crate's `default_client_config` helper (which in turn
   * reads `ClientConfig::new(...)` in `kmail-core`).
   *
   * Use this when constructing a `JsClientConfig` in renderer
   * code so the desktop shell picks up SDK-level default changes
   * automatically on the next napi rebuild. Do NOT hard-code
   * literal defaults in JS — that re-introduces the cross-binding
   * drift bug the helper was designed to eliminate.
   */
  async defaultClientConfig(
    bffUrl: string,
    bearerToken: string,
    databasePath: string,
  ): Promise<JsClientConfig> {
    try {
      return await this.bridge.defaultClientConfig(
        bffUrl,
        bearerToken,
        databasePath,
      );
    } catch (err) {
      throw parseKMailError(err);
    }
  }
}

/**
 * Resolve the `KMailDesktopClient` instance the renderer should
 * use.
 *
 *   - Production: returns a singleton client backed by
 *     `window.kmail` (the contextBridge surface from
 *     `preload.ts`). `window.kmail` is set exactly once by the
 *     preload script and never replaced for the lifetime of the
 *     renderer process, so caching the wrapper is safe.
 *   - Tests: accepts an injected `KMailBridge` stub. Each stub
 *     identity is wrapped in its own `KMailDesktopClient`
 *     (cached via `WeakMap` so the same stub instance returns
 *     the same client on repeat calls — required for any
 *     consumer that places the result in a `useEffect`
 *     dependency array).
 *
 * **Stability contract.** All three renderer consumers
 * (`App.tsx`, `pages/Inbox.tsx`, `pages/Compose.tsx`) place the
 * returned client into `useEffect` dependency arrays. If
 * `useKMail()` ever returned a fresh object on every call,
 * the effect would re-fire on every render and the resulting
 * async state-setter cascade would silently saturate the IPC
 * channel — React doesn't surface this as `Maximum update
 * depth exceeded` because the state updates come from async
 * callbacks. The cached references below are what keeps the
 * renderer from melting.
 *
 * Throws `KMailError(kind: 'internal')` if `window.kmail` is
 * missing — that means the preload script never ran, which is
 * a fatal configuration error rather than a recoverable state.
 */
let cachedDefaultClient: KMailDesktopClient | null = null;
const stubClientCache = new WeakMap<KMailBridge, KMailDesktopClient>();

export function useKMail(stub?: KMailBridge): KMailDesktopClient {
  if (stub) {
    const existing = stubClientCache.get(stub);
    if (existing) return existing;
    const fresh = new KMailDesktopClient(stub);
    stubClientCache.set(stub, fresh);
    return fresh;
  }
  if (cachedDefaultClient) return cachedDefaultClient;
  if (typeof window === 'undefined' || !window.kmail) {
    throw new KMailError(
      'internal',
      '[INTERNAL]',
      '[INTERNAL] window.kmail is missing — preload script did not run',
    );
  }
  cachedDefaultClient = new KMailDesktopClient(window.kmail);
  return cachedDefaultClient;
}

/**
 * Reset the cached default client. Test-only escape hatch for
 * suites that need to re-evaluate `window.kmail` between
 * specs (e.g. simulating missing preload). Production code
 * never calls this.
 */
export function __resetKMailCacheForTests(): void {
  cachedDefaultClient = null;
}
