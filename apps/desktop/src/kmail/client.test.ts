// Unit tests for the renderer-side `KMailDesktopClient` wrapper.
//
// These tests don't spin up Electron — they inject a stub
// `KMailBridge` implementation and verify:
//
//   1. The wire-format draft encoder matches the JMAP RFC 8621
//      shape that the Rust SDK / Swift facade / Kotlin facade
//      all emit (load-bearing cross-binding parity).
//   2. Errors with known SDK tag prefixes parse into typed
//      `KMailError` instances; unknown prefixes fall through to
//      `kind: 'internal'`.
//   3. Method dispatch goes through the bridge — no silent
//      swallow / no double-encoding.

import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  __resetKMailCacheForTests,
  encodeWireFormatDraft,
  KMailDesktopClient,
  useKMail,
} from './client';
import type { EmailDraft } from './client';
import { KMailError, parseKMailError } from './errors';
import type { KMailBridge } from './preload';

function makeStubBridge(
  overrides: Partial<KMailBridge> = {},
): KMailBridge {
  return {
    open: vi.fn().mockResolvedValue(undefined),
    close: vi.fn().mockResolvedValue(undefined),
    sync: vi.fn().mockResolvedValue({
      mailboxesUpserted: 0n,
      mailboxesDestroyed: 0n,
      emailsCreated: 0n,
      emailsUpdated: 0n,
      emailsDestroyed: 0n,
      pendingActionsApplied: 0n,
      pendingActionsFailed: 0n,
      pendingActionsDeferred: 0n,
    }),
    setBearerToken: vi.fn().mockResolvedValue(undefined),
    invalidateSession: vi.fn().mockResolvedValue(undefined),
    cachedMailboxes: vi.fn().mockResolvedValue([]),
    cachedEmails: vi.fn().mockResolvedValue([]),
    sendEmail: vi.fn().mockResolvedValue('email-id-1'),
    enqueueSetKeywords: vi.fn().mockResolvedValue(undefined),
    notify: vi.fn().mockResolvedValue(undefined),
    ...overrides,
  };
}

describe('encodeWireFormatDraft', () => {
  it('emits the full JMAP RFC 8621 key set, including empty defaults', () => {
    // Mirror of `testEmailDraftEncodesToRustWireFormat` in
    // apps/ios/Tests/.../KMailIntegrationTests.swift:233 and
    // the Kotlin integration test at apps/android/.../KMailIntegrationTests.kt:300.
    // Every binding MUST produce the same key set so the BFF
    // observability dashboards diff payloads cleanly.
    const draft: EmailDraft = {
      mailboxIds: { 'mb-drafts': true },
      from: [{ name: 'Alice', email: 'alice@kmail.test' }],
      to: [{ name: 'Bob', email: 'bob@kmail.test' }],
      subject: 'hello',
      textBody: 'hi bob',
    };
    const json = encodeWireFormatDraft(draft);
    expect(json).toContain('"mailboxIds"');
    expect(json).toContain('"textBody"');
    expect(json).toContain('"inReplyTo"');
    expect(json).toContain('"cc"');
    expect(json).toContain('"bcc"');
    expect(json).toContain('"replyTo"');
    expect(json).toContain('"references"');
    expect(json).toContain('"htmlBody"');
    expect(json).toContain('alice@kmail.test');
    // Parsing as JSON must round-trip with the empty-default
    // shape — `cc`, `bcc`, `replyTo`, `references`, `inReplyTo`
    // present as empty arrays; `htmlBody` present as null.
    const parsed = JSON.parse(json) as Record<string, unknown>;
    expect(parsed.cc).toEqual([]);
    expect(parsed.bcc).toEqual([]);
    expect(parsed.replyTo).toEqual([]);
    expect(parsed.references).toEqual([]);
    expect(parsed.inReplyTo).toEqual([]);
    expect(parsed.htmlBody).toBeNull();
  });

  it('preserves explicit non-empty defaults', () => {
    const draft: EmailDraft = {
      mailboxIds: { 'mb-1': true },
      from: [{ name: 'A', email: 'a@x' }],
      to: [{ name: 'B', email: 'b@y' }],
      cc: [{ name: 'C', email: 'c@z' }],
      replyTo: [{ name: 'A', email: 'a@x' }],
      inReplyTo: ['msg-99'],
      subject: 's',
      textBody: 't',
      htmlBody: '<p>t</p>',
    };
    const parsed = JSON.parse(encodeWireFormatDraft(draft)) as Record<
      string,
      unknown
    >;
    expect(parsed.cc).toHaveLength(1);
    expect(parsed.replyTo).toHaveLength(1);
    expect(parsed.inReplyTo).toEqual(['msg-99']);
    expect(parsed.htmlBody).toBe('<p>t</p>');
  });
});

describe('parseKMailError', () => {
  it('maps every known SDK error tag to its typed kind', () => {
    const cases: Array<readonly [string, string]> = [
      ['[STORE] sqlite locked', 'store'],
      ['[TRANSPORT] connection reset', 'transport'],
      ['[AUTH] expired token', 'auth'],
      ['[FORBIDDEN] no such tenant', 'forbidden'],
      ['[NOT_FOUND] mailbox', 'not-found'],
      ['[RATE_LIMIT] retry after 30s', 'rate-limit'],
      ['[JMAP] anchorNotFound', 'jmap'],
      ['[PROTOCOL] malformed session', 'protocol'],
      ['[HTTP_CLIENT] 502', 'http-client'],
      ['[SYNC_DIVERGED] state token mismatch', 'sync-diverged'],
      ['[DECRYPTION] AES tag mismatch', 'decryption'],
      ['[KDF] HKDF expand failed', 'kdf'],
      ['[KEYSTORE] missing handle', 'keystore'],
      ['[ARG] negative cache size', 'invalid-argument'],
      ['[CANCELLED] user aborted', 'cancelled'],
      ['[INTERNAL] preload missing', 'internal'],
    ];
    for (const [message, kind] of cases) {
      const parsed = parseKMailError(new Error(message));
      expect(parsed).toBeInstanceOf(KMailError);
      expect(parsed.kind).toBe(kind);
      expect(parsed.message).toBe(message);
    }
  });

  it('falls back to internal kind for unknown prefixes', () => {
    const parsed = parseKMailError(new Error('mystery failure'));
    expect(parsed.kind).toBe('internal');
    expect(parsed.tag).toBe('[INTERNAL]');
  });

  it('does not re-wrap an existing KMailError', () => {
    const original = new KMailError('rate-limit', '[RATE_LIMIT]', 'x');
    const reparsed = parseKMailError(original);
    expect(reparsed).toBe(original);
  });
});

describe('KMailDesktopClient', () => {
  it('routes sendDraft through encodeWireFormatDraft + bridge.sendEmail', async () => {
    const bridge = makeStubBridge();
    const client = new KMailDesktopClient(bridge);
    const id = await client.sendDraft({
      mailboxIds: { 'mb-1': true },
      from: [{ name: 'A', email: 'a@x' }],
      to: [{ name: 'B', email: 'b@y' }],
      subject: 'hi',
    });
    expect(id).toBe('email-id-1');
    expect(bridge.sendEmail).toHaveBeenCalledOnce();
    const [draftJson] = (bridge.sendEmail as ReturnType<typeof vi.fn>).mock
      .calls[0]!;
    // Bridge must see the wire-format encoded JSON, not the raw
    // object — re-encoding on the main process side would break
    // cross-binding parity.
    expect(typeof draftJson).toBe('string');
    expect(JSON.parse(draftJson)).toMatchObject({
      mailboxIds: { 'mb-1': true },
      cc: [],
      bcc: [],
      replyTo: [],
      references: [],
      inReplyTo: [],
    });
  });

  it('rethrows bridge errors as typed KMailError', async () => {
    const bridge = makeStubBridge({
      sync: vi.fn().mockRejectedValue(new Error('[RATE_LIMIT] try later')),
    });
    const client = new KMailDesktopClient(bridge);
    await expect(client.sync()).rejects.toBeInstanceOf(KMailError);
    await expect(client.sync()).rejects.toMatchObject({
      kind: 'rate-limit',
      tag: '[RATE_LIMIT]',
    });
  });

  it('routes setKeywords through JSON.stringify and the bridge', async () => {
    const bridge = makeStubBridge();
    const client = new KMailDesktopClient(bridge);
    await client.setKeywords('email-1', { $seen: true, $flagged: false });
    expect(bridge.enqueueSetKeywords).toHaveBeenCalledWith(
      'email-1',
      JSON.stringify({ $seen: true, $flagged: false }),
    );
  });
});

describe('useKMail stability contract', () => {
  afterEach(() => {
    __resetKMailCacheForTests();
    // Clear any window.kmail injected by a test.
    if (typeof window !== 'undefined') {
      delete (window as unknown as { kmail?: KMailBridge }).kmail;
    }
  });

  it('returns the same client on repeat calls for the same stub bridge', () => {
    // Load-bearing: App.tsx / Inbox.tsx / Compose.tsx place the
    // result into useEffect dependency arrays. A fresh reference
    // every render would create an infinite re-render loop.
    const bridge = makeStubBridge();
    const c1 = useKMail(bridge);
    const c2 = useKMail(bridge);
    expect(c1).toBe(c2);
  });

  it('returns the same client on repeat calls in production mode (window.kmail backed)', () => {
    const bridge = makeStubBridge();
    (window as unknown as { kmail?: KMailBridge }).kmail = bridge;
    const c1 = useKMail();
    const c2 = useKMail();
    expect(c1).toBe(c2);
  });

  it('returns distinct clients for distinct stub bridges', () => {
    const c1 = useKMail(makeStubBridge());
    const c2 = useKMail(makeStubBridge());
    expect(c1).not.toBe(c2);
  });

  it('throws a typed KMailError when window.kmail is missing', () => {
    expect(() => useKMail()).toThrowError(KMailError);
  });
});
