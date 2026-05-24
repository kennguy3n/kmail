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
    await this.bridge.close();
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
}

/**
 * Resolve the `KMailDesktopClient` instance the renderer should
 * use.
 *
 *   - Production: returns a client backed by `window.kmail`
 *     (the contextBridge surface from `preload.ts`).
 *   - Tests: accepts an injected `KMailBridge` stub.
 *
 * Throws `KMailError(kind: 'internal')` if `window.kmail` is
 * missing — that means the preload script never ran, which is
 * a fatal configuration error rather than a recoverable state.
 */
export function useKMail(stub?: KMailBridge): KMailDesktopClient {
  if (stub) return new KMailDesktopClient(stub);
  if (typeof window === 'undefined' || !window.kmail) {
    throw new KMailError(
      'internal',
      '[INTERNAL]',
      '[INTERNAL] window.kmail is missing — preload script did not run',
    );
  }
  return new KMailDesktopClient(window.kmail);
}
