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
│       ├── crypto/             # AES-256-GCM, HKDF, KeyStore trait
│       │   ├── aead.rs
│       │   ├── kdf.rs
│       │   └── keystore.rs
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
`[SYNC_DIVERGED]`, `[DECRYPTION]`, `[KDF]`, `[KEYSTORE]`,
`[ARG]`, `[CANCELLED]`. The Electron side parses the prefix to
construct typed exception classes. A unit test
(`error_prefixes_are_stable`) keeps the contract from drifting.

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

The SDK enforces the contract documented in `ARCHITECTURE.md`
§5. Vault decryption is keyed off MLS material that the platform
shell hands in through the `KeyStore` trait:

- iOS shell → Keychain Services
- Android shell → Android Keystore
- Electron shell → OS keyring (Secret Service / Keychain /
  Windows Credential Manager) via `keyring`

The Rust side never touches platform secure storage directly; it
only ever sees opaque `KeyMaterial` byte slices via the
`KeyStore::fetch_key_material` boundary.

The full vault decrypt path (vault envelope → MLS exporter →
DEK unwrap → AES-256-GCM open) lands in the next SDK PR; the
primitives (`crypto::aead`, `crypto::kdf`, `crypto::keystore`)
are already in place and verified against the published test
vectors.
