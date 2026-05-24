// Typed exception classes parsed from the napi-rs error tags.
//
// The napi binding at `sdk/kmail-napi/src/lib.rs::napi_err`
// prefixes every error message with a stable tag like `[STORE]`
// or `[RATE_LIMIT]`. The Electron main IPC handlers preserve
// these prefixes via `sanitiseError(...)` and surface them to
// the renderer as a normal `Error.message`. This module converts
// the prefix back into a typed exception class so renderer code
// can `instanceof`-check rather than string-match.
//
// **The set of prefixes here is load-bearing.** The Rust-side
// `error_prefixes_are_stable` test asserts these exact strings,
// and the Electron-side `sanitiseError(...)` rejects unknown
// prefixes as `[INTERNAL]`. Adding a new SDK error variant
// requires updates in all three places.

export type KMailErrorKind =
  | 'store'
  | 'transport'
  | 'auth'
  | 'forbidden'
  | 'not-found'
  | 'rate-limit'
  | 'jmap'
  | 'protocol'
  | 'http-client'
  | 'sync-diverged'
  | 'decryption'
  | 'kdf'
  | 'keystore'
  | 'invalid-argument'
  | 'cancelled'
  | 'internal';

export class KMailError extends Error {
  public readonly kind: KMailErrorKind;
  public readonly tag: string;

  constructor(kind: KMailErrorKind, tag: string, message: string) {
    super(message);
    this.kind = kind;
    this.tag = tag;
    this.name = 'KMailError';
  }
}

const PREFIX_KIND: ReadonlyArray<readonly [string, KMailErrorKind]> = [
  ['[STORE]', 'store'],
  ['[TRANSPORT]', 'transport'],
  ['[AUTH]', 'auth'],
  ['[FORBIDDEN]', 'forbidden'],
  ['[NOT_FOUND]', 'not-found'],
  ['[RATE_LIMIT]', 'rate-limit'],
  ['[JMAP]', 'jmap'],
  ['[PROTOCOL]', 'protocol'],
  ['[HTTP_CLIENT]', 'http-client'],
  ['[SYNC_DIVERGED]', 'sync-diverged'],
  ['[DECRYPTION]', 'decryption'],
  ['[KDF]', 'kdf'],
  ['[KEYSTORE]', 'keystore'],
  ['[ARG]', 'invalid-argument'],
  ['[CANCELLED]', 'cancelled'],
  ['[INTERNAL]', 'internal'],
] as const;

/**
 * Parse the tag-prefixed error message produced by the Electron
 * IPC bridge into a `KMailError` of the appropriate kind. Falls
 * back to `internal` for any prefix the renderer doesn't
 * recognise — that case indicates a tag drift between Rust /
 * Electron / renderer that should fail loudly in dev.
 */
export function parseKMailError(err: unknown): KMailError {
  if (err instanceof KMailError) return err;
  const message =
    err instanceof Error
      ? err.message
      : typeof err === 'string'
        ? err
        : String(err);
  for (const [prefix, kind] of PREFIX_KIND) {
    if (message.startsWith(prefix)) {
      return new KMailError(kind, prefix, message);
    }
  }
  return new KMailError('internal', '[INTERNAL]', message);
}
