# KMail SDK

`kmail-sdk` is the cross-platform Rust workspace that powers the
iOS, Android, and Electron desktop KMail clients. The native
shells are thin presentation layers; the SDK owns every byte of
protocol state, every offline-cached row, and every crypto
operation that does not live on the server.

## Workspace layout

```
sdk/
├── Cargo.toml                  # workspace + pinned dependencies
├── kmail-core/                 # protocol, sync, crypto, push, cache
│   └── src/
│       ├── cache.rs            # attachment LRU over SQLite
│       ├── client.rs           # KMailClient façade + delta-pull
│       ├── crypto/             # AES-256-GCM, HKDF, MLS bridge, vault + Confidential Send envelopes
│       │   ├── mod.rs          # re-exports + KeyMaterial wrapper
│       │   ├── aead.rs         # AES-256-GCM primitive
│       │   ├── kdf.rs          # HKDF-SHA256 primitive
│       │   ├── keystore.rs     # KeyStore trait + InMemoryKeyStore
│       │   ├── mls.rs          # MlsKeyProvider trait + StaticMlsKeyProvider
│       │   ├── confidential.rs # Confidential Send seal / open (MLS-leaf-keyed KEK + random DEK)
│       │   └── vault.rs        # Zero-Access Vault seal / open (folder-master-keyed)
│       ├── error.rs            # typed Error taxonomy
│       ├── jmap/               # JMAP transport + request/response
│       │   ├── ops.rs          # typed JmapClient methods
│       │   ├── request.rs      # custom Serialize for triplets
│       │   ├── response.rs     # custom Deserialize + typed payloads
│       │   └── transport.rs    # reqwest + Bearer + backoff
│       ├── models.rs           # JMAP wire types + KMail extensions
│       ├── push.rs             # APNs / FCM / WebPush + StateChange
│       └── sync/               # SQLite migrations + repos
│           ├── actions.rs      # outbox queue
│           ├── email_repo.rs
│           ├── mailbox_repo.rs
│           ├── schema.rs
│           ├── state.rs        # JMAP state token CRUD
│           └── store.rs
├── kmail-ffi/                  # UniFFI bindings (iOS / Android)
│   └── src/lib.rs
├── kmail-napi/                 # napi-rs bindings (Electron)
│   ├── build.rs
│   ├── package.json
│   └── src/lib.rs
└── kmail-cli/                  # internal debug CLI
    └── src/main.rs
```

## Dependencies

Pinned at the workspace root (`sdk/Cargo.toml`) so every crate
shares the same versions:

| Crate           | Version | Purpose                                    |
| --------------- | ------- | ------------------------------------------ |
| `tokio`         | 1.40    | async runtime (multi-thread + rt features) |
| `reqwest`       | 0.12    | HTTP client (rustls TLS)                   |
| `rusqlite`      | 0.32    | SQLite bindings (bundled SQLite build)     |
| `serde`         | 1.x     | data binding                               |
| `aes-gcm`       | 0.10    | AES-256-GCM AEAD                           |
| `hkdf`          | 0.12    | HKDF-SHA256                                |
| `uniffi`        | 0.28    | proc-macro Swift / Kotlin bindings         |
| `napi`/`-derive`| 2.16    | Node-API addon bindings                    |
| `clap`          | 4.x     | CLI parser (kmail-cli only)                |

## Build matrix

| Target               | Command                                                                                                  |
| -------------------- | -------------------------------------------------------------------------------------------------------- |
| Host (dev / CI)      | `cargo build --workspace --all-targets`                                                                  |
| iOS device           | `cargo build -p kmail-ffi --target aarch64-apple-ios --release` *(follow-up PR)*                         |
| iOS simulator (arm)  | `cargo build -p kmail-ffi --target aarch64-apple-ios-sim --release` *(follow-up PR)*                     |
| iOS simulator (x64)  | `cargo build -p kmail-ffi --target x86_64-apple-ios --release` *(follow-up PR)*                          |
| Android arm64        | `cargo build -p kmail-ffi --target aarch64-linux-android --release` *(follow-up PR)*                     |
| Android armv7        | `cargo build -p kmail-ffi --target armv7-linux-androideabi --release` *(follow-up PR)*                   |
| Android x86_64       | `cargo build -p kmail-ffi --target x86_64-linux-android --release` *(follow-up PR)*                      |
| Desktop (napi-rs)    | `cd sdk/kmail-napi && npx @napi-rs/cli build --release` *(follow-up PR for multi-target sweep)*          |

Cross-compile sweeps for iOS, Android, and napi targets land in
follow-up PRs that wire the XCFramework, AAR, and `.node` bundling
into CI.

## UniFFI binding generation

`kmail-ffi` uses UniFFI 0.28 proc-macros (no `.udl` file). The
surface is small enough that proc-macros are simpler and let the
compiler check binding shape against the implementation.

To regenerate the Swift / Kotlin packages:

```bash
cd sdk/kmail-ffi
cargo run --bin uniffi-bindgen -- generate --library target/debug/libkmail_ffi.{so,dylib} --language swift --out-dir bindings/swift
cargo run --bin uniffi-bindgen -- generate --library target/debug/libkmail_ffi.{so,dylib} --language kotlin --out-dir bindings/kotlin
```

The platform shell repos consume these bindings as a Swift package
and a Gradle module respectively.

## napi-rs build

`kmail-napi` exports the same `KMailClient` surface via N-API.
`@napi-rs/cli` produces a `kmail-sdk-native.${platform}-${arch}.node`
binary per target; the package's `napi.triples` block in
`package.json` enumerates the desktop matrix (macOS x64/arm64,
Linux x64/arm64, Windows x64).

Error tagging convention: errors cross the FFI boundary as
`napi::Error` with `reason = "[TYPE] description"` where `[TYPE]`
is one of `[STORE]`, `[TRANSPORT]`, `[AUTH]`, `[FORBIDDEN]`,
`[NOT_FOUND]`, `[RATE_LIMIT]`, `[JMAP]`, `[PROTOCOL]`,
`[HTTP_CLIENT]`, `[SYNC_DIVERGED]`, `[DECRYPTION]`, `[KDF]`,
`[KEYSTORE]`, `[ARG]`, `[CANCELLED]`. The Electron side parses
the prefix to construct typed exception classes. A unit test
(`error_prefixes_are_stable`) keeps the contract from drifting,
so adding a new prefix here requires adding the matching arm in
`kmail-napi/src/lib.rs` and the assertion in the unit test.

## Debug CLI

`kmail-cli` is the SDK's internal debug surface. It drives the
real `KMailClient` directly so engineers can reproduce sync
behaviour without going through a native shell.

```bash
cargo run -p kmail-cli -- session   --bff https://bff.example.com --token "$TOKEN"
cargo run -p kmail-cli -- sync      --bff https://bff.example.com --token "$TOKEN" --db /tmp/kmail.db
cargo run -p kmail-cli -- mailboxes --db /tmp/kmail.db
cargo run -p kmail-cli -- emails    --db /tmp/kmail.db --mailbox <id> --limit 50
cargo run -p kmail-cli -- email     --bff https://bff.example.com --token "$TOKEN" --db /tmp/kmail.db --id <email-id>
cargo run -p kmail-cli -- doctor    --db /tmp/kmail.db
```

`doctor` prints the schema version, row counts, and SQLite
version — handy for diagnosing a stale on-device cache.

## CI

The `sdk` job in `.github/workflows/ci.yml` runs
`cargo fmt --check`, `cargo clippy --workspace -- -D warnings`,
`cargo test --workspace --all-targets`, and a release sanity
build. The job is gated on a `sdk` paths-filter so unrelated PRs
do not pay the Rust compile cost, but is rolled into the
aggregate `CI Status` check that branch protection requires.

## Encryption

The SDK implements the contract documented in `ARCHITECTURE.md`
§5. Two privacy modes have first-class envelope types, both
sealed and opened entirely on-device:

| Envelope                 | Crate path                       | Wrapping key derivation                                 | Per-message material                  |
| ------------------------ | -------------------------------- | ------------------------------------------------------- | ------------------------------------- |
| Zero-Access Vault        | `kmail_core::crypto::vault`      | HKDF-SHA256(folder master key, salt = nonce, label)     | 96-bit nonce sampled from `OsRng`     |
| Confidential Send        | `kmail_core::crypto::confidential`| HKDF-SHA256(MLS leaf secret, salt = `kek_salt`, label) | Random DEK (32 bytes), random `kek_salt`|

Both envelopes use AES-256-GCM under the hood (`crypto::aead`)
and HKDF-SHA256 for key derivation (`crypto::kdf`). All
derived keys and the random DEK are zeroized after use via
`zeroize::Zeroize`; `KeyMaterial` is the canonical
`ZeroizeOnDrop` wrapper for any key bytes that need to live
across `await` points.

### MLS exporter bridge

The SDK never derives MLS material itself — that's the KChat
MLS SDK's job (per the do-not-do rule "do not build a parallel
email-only key hierarchy"). The platform shell hands MLS
exporter secrets in through the `MlsKeyProvider` trait
(`crypto::mls`):

```rust
pub trait MlsKeyProvider: Send + Sync {
    fn confidential_send_leaf_secret(&self, recipient_user_id: &str)
        -> Result<KeyMaterial>;
    fn vault_folder_master_secret(&self, folder_id: &str)
        -> Result<KeyMaterial>;
}
```

- `StaticMlsKeyProvider` (in the same module) is the test-only
  in-memory implementation. Production providers wrap the
  KChat MLS SDK and run in whichever process owns the MLS
  tree.
- FFI-side adapters (`kmail-ffi::ForeignMlsKeyProvider`,
  `kmail-napi`'s equivalent) validate the 32-byte contract at
  the boundary and surface mismatches as
  `Error::KeyStore`. Wrong-length foreign-callback returns are
  zeroized before being dropped — see the FFI tests for the
  exact discipline.

### KeyStore (session blobs)

`KeyStore` (`crypto::keystore`) is unrelated to MLS material —
it's the trait the SDK uses to persist OAuth / JMAP session
blobs between launches. The default `InMemoryKeyStore` is
fine for tests and the kmail-cli; production shells will plug
in platform-native bridges in their respective follow-up PRs
(Keychain Services / Android Keystore / OS keyring).

The Rust side never touches platform secure storage directly;
it only ever sees opaque `KeyMaterial` byte slices via the
`MlsKeyProvider` and `KeyStore` boundaries.
